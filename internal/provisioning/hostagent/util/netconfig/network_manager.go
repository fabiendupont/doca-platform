// Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netconfig

import (
	"fmt"

	"github.com/godbus/dbus/v5"
	"github.com/nvidia/doca-platform/internal/provisioning/hostagent/util"
	"k8s.io/klog/v2"
)

const (
	// NetworkManager D-Bus service and paths
	nmService       = "org.freedesktop.NetworkManager"
	nmPath          = "/org/freedesktop/NetworkManager"
	nmSettingsPath  = "/org/freedesktop/NetworkManager/Settings"
	nmSettingsIface = "org.freedesktop.NetworkManager.Settings"
	nmConnIface     = "org.freedesktop.NetworkManager.Connection"
	nmIface         = "org.freedesktop.NetworkManager"
)

// NetworkManagerBackend implements Backend interface using NetworkManager via D-Bus
type NetworkManagerBackend struct {
	conn *dbus.Conn
}

// NewNetworkManagerBackend creates a new NetworkManager backend
func NewNetworkManagerBackend() Backend {
	return &NetworkManagerBackend{}
}

// Name returns the human-readable name of the backend
func (n *NetworkManagerBackend) Name() string {
	return string(BackendTypeNetworkManager)
}

// IsAvailable checks if NetworkManager is available on the system
func (n *NetworkManagerBackend) IsAvailable() bool {
	return HasNetworkManager()
}

// getDBusConn returns a D-Bus system bus connection, creating it if needed
func (n *NetworkManagerBackend) getDBusConn() (*dbus.Conn, error) {
	if n.conn == nil {
		conn, err := dbus.SystemBus()
		if err != nil {
			return nil, fmt.Errorf("failed to connect to system bus: %w", err)
		}
		n.conn = conn
	}
	return n.conn, nil
}

// makeConnectionSettings creates a D-Bus connection settings map
func makeConnectionSettings(connName, connType, interfaceName string) map[string]map[string]dbus.Variant {
	settings := map[string]map[string]dbus.Variant{
		"connection": {
			"id":             dbus.MakeVariant(connName),
			"type":           dbus.MakeVariant(connType),
			"autoconnect":    dbus.MakeVariant(true),
		},
	}

	if interfaceName != "" {
		settings["connection"]["interface-name"] = dbus.MakeVariant(interfaceName)
	}

	// Initialize type-specific settings
	switch connType {
	case "802-3-ethernet":
		settings["802-3-ethernet"] = make(map[string]dbus.Variant)
		settings["ipv4"] = map[string]dbus.Variant{
			"method": dbus.MakeVariant("auto"),
		}
		settings["ipv6"] = map[string]dbus.Variant{
			"method": dbus.MakeVariant("auto"),
		}
	case "bridge":
		settings["bridge"] = make(map[string]dbus.Variant)
		settings["ipv4"] = map[string]dbus.Variant{
			"method": dbus.MakeVariant("disabled"),
		}
		settings["ipv6"] = map[string]dbus.Variant{
			"method": dbus.MakeVariant("disabled"),
		}
	}

	return settings
}

// ConfigurePFInterfaces configures physical function network interfaces using NetworkManager
// Returns (needsApply, error) where needsApply indicates if changes were made
func (n *NetworkManagerBackend) ConfigurePFInterfaces(pciAddress string, portConfigs []PortConfig) (bool, error) {
	pciHelper := util.NewPCIHelper(pciAddress)
	needsApply := false

	for _, portConfig := range portConfigs {
		// Skip if no configuration needed
		if portConfig.MTU == nil && portConfig.DHCP == nil {
			continue
		}

		pf := pciHelper.PF(int(portConfig.PortNumber))
		interfaceName, err := pf.InterfaceName()
		if err != nil {
			return false, fmt.Errorf("failed to get PF%d interface name: %w", portConfig.PortNumber, err)
		}

		// Connection name follows convention: dpu-pf<N>-<interface-name>
		connName := fmt.Sprintf("dpu-pf%d-%s", portConfig.PortNumber, interfaceName)

		// Check if connection exists, create if not
		exists, err := n.connectionExists(connName)
		if err != nil {
			return false, fmt.Errorf("failed to check connection %s existence: %w", connName, err)
		}

		if !exists {
			klog.V(3).Infof("Creating NetworkManager connection %s for interface %s", connName, interfaceName)
			if err := n.createConnection(connName, "802-3-ethernet", interfaceName); err != nil {
				return false, fmt.Errorf("failed to create connection %s: %w", connName, err)
			}
			needsApply = true
		}

		// Get connection for modifications
		conn, err := n.getConnectionByName(connName)
		if err != nil {
			return false, fmt.Errorf("failed to get connection %s: %w", connName, err)
		}

		settings, err := n.getConnectionSettings(conn)
		if err != nil {
			return false, fmt.Errorf("failed to get settings for %s: %w", connName, err)
		}

		modified := false

		// Configure MTU if specified
		if portConfig.MTU != nil {
			currentMTU, err := util.GetCurrentMTU(interfaceName)
			if err != nil {
				return false, fmt.Errorf("failed to get current MTU for %s: %w", interfaceName, err)
			}

			if currentMTU != int(*portConfig.MTU) {
				klog.Infof("%s MTU mismatch (current=%d, desired=%d)", interfaceName, currentMTU, *portConfig.MTU)
				if settings["802-3-ethernet"] == nil {
					settings["802-3-ethernet"] = make(map[string]dbus.Variant)
				}
				settings["802-3-ethernet"]["mtu"] = dbus.MakeVariant(uint32(*portConfig.MTU))
				modified = true
			}
		}

		// Configure DHCP if specified
		if portConfig.DHCP != nil {
			currentDHCP, err := n.IsDHCPConfigured(interfaceName)
			if err != nil {
				return false, fmt.Errorf("failed to determine DHCP configuration for %s: %w", interfaceName, err)
			}

			if currentDHCP != *portConfig.DHCP {
				klog.Infof("%s DHCP mismatch (current=%v, desired=%v)", interfaceName, currentDHCP, *portConfig.DHCP)
				method := "manual"
				if *portConfig.DHCP {
					method = "auto"
				}
				if settings["ipv4"] == nil {
					settings["ipv4"] = make(map[string]dbus.Variant)
				}
				settings["ipv4"]["method"] = dbus.MakeVariant(method)
				modified = true
			}
		}

		// Update connection if modified
		if modified {
			if err := n.updateConnection(conn, settings); err != nil {
				return false, fmt.Errorf("failed to update connection %s: %w", connName, err)
			}
			needsApply = true
		}
	}

	return needsApply, nil
}

// ConfigureBridgeMTU configures the MTU for a bridge interface using NetworkManager
// Returns (needsApply, error) where needsApply indicates if changes were made
func (n *NetworkManagerBackend) ConfigureBridgeMTU(bridgeName string, mtu int) (bool, error) {
	// Check if changes are needed first
	needsApply, err := n.checkBridgeMTUChangeNeeded(bridgeName, mtu)
	if err != nil {
		return false, fmt.Errorf("failed to check bridge MTU state: %w", err)
	}

	if !needsApply {
		klog.V(3).Infof("Bridge %s and members already have correct MTU %d", bridgeName, mtu)
		return false, nil
	}

	// Get bridge member interfaces
	memberNames, err := util.GetBridgeMembers(bridgeName)
	if err != nil {
		return false, fmt.Errorf("failed to get bridge members for %s: %w", bridgeName, err)
	}

	// Configure bridge connection
	bridgeConnName := fmt.Sprintf("dpu-bridge-%s", bridgeName)
	exists, err := n.connectionExists(bridgeConnName)
	if err != nil {
		return false, fmt.Errorf("failed to check bridge connection existence: %w", err)
	}

	if !exists {
		klog.V(3).Infof("Creating NetworkManager bridge connection %s", bridgeConnName)
		if err := n.createConnection(bridgeConnName, "bridge", bridgeName); err != nil {
			return false, fmt.Errorf("failed to create bridge connection: %w", err)
		}
	}

	// Set bridge MTU
	bridgeConn, err := n.getConnectionByName(bridgeConnName)
	if err != nil {
		return false, fmt.Errorf("failed to get bridge connection: %w", err)
	}

	bridgeSettings, err := n.getConnectionSettings(bridgeConn)
	if err != nil {
		return false, fmt.Errorf("failed to get bridge settings: %w", err)
	}

	if bridgeSettings["bridge"] == nil {
		bridgeSettings["bridge"] = make(map[string]dbus.Variant)
	}
	// Note: Bridge MTU is set on the bridge interface itself, not on the bridge settings
	// We set it on member interfaces instead

	if err := n.updateConnection(bridgeConn, bridgeSettings); err != nil {
		return false, fmt.Errorf("failed to update bridge connection: %w", err)
	}

	// Configure MTU for all bridge member interfaces
	for _, memberName := range memberNames {
		memberConnName := fmt.Sprintf("dpu-bridge-member-%s", memberName)
		exists, err := n.connectionExists(memberConnName)
		if err != nil {
			return false, fmt.Errorf("failed to check member connection %s existence: %w", memberConnName, err)
		}

		if !exists {
			klog.V(3).Infof("Creating NetworkManager connection %s for bridge member %s", memberConnName, memberName)
			if err := n.createConnection(memberConnName, "802-3-ethernet", memberName); err != nil {
				return false, fmt.Errorf("failed to create member connection %s: %w", memberConnName, err)
			}
		}

		// Get member connection
		memberConn, err := n.getConnectionByName(memberConnName)
		if err != nil {
			return false, fmt.Errorf("failed to get member connection %s: %w", memberConnName, err)
		}

		memberSettings, err := n.getConnectionSettings(memberConn)
		if err != nil {
			return false, fmt.Errorf("failed to get member settings for %s: %w", memberConnName, err)
		}

		// Set member interface MTU
		if memberSettings["802-3-ethernet"] == nil {
			memberSettings["802-3-ethernet"] = make(map[string]dbus.Variant)
		}
		memberSettings["802-3-ethernet"]["mtu"] = dbus.MakeVariant(uint32(mtu))

		// Set member as bridge slave
		if memberSettings["connection"] == nil {
			memberSettings["connection"] = make(map[string]dbus.Variant)
		}
		memberSettings["connection"]["master"] = dbus.MakeVariant(bridgeName)
		memberSettings["connection"]["slave-type"] = dbus.MakeVariant("bridge")

		if err := n.updateConnection(memberConn, memberSettings); err != nil {
			return false, fmt.Errorf("failed to update member %s: %w", memberName, err)
		}
	}

	return true, nil
}

// ApplyConfiguration activates connections to apply pending configuration changes
func (n *NetworkManagerBackend) ApplyConfiguration() error {
	klog.Infof("Activating NetworkManager connections")

	conn, err := n.getDBusConn()
	if err != nil {
		return err
	}

	// List all connections
	connections, err := n.listConnections()
	if err != nil {
		return fmt.Errorf("failed to list connections: %w", err)
	}

	// Activate all dpu-* connections
	for _, connPath := range connections {
		connObj := conn.Object(nmService, connPath)

		var settings map[string]map[string]dbus.Variant
		err := connObj.Call(nmConnIface+".GetSettings", 0).Store(&settings)
		if err != nil {
			continue
		}

		// Get connection ID
		idVariant, ok := settings["connection"]["id"]
		if !ok {
			continue
		}

		connID, ok := idVariant.Value().(string)
		if !ok {
			continue
		}

		// Only activate dpu-* connections
		if len(connID) < 4 || connID[:4] != "dpu-" {
			continue
		}

		klog.V(3).Infof("Activating connection %s", connID)
		// Try to activate, but don't fail if already active or device unavailable
		if err := n.activateConnection(connPath); err != nil {
			klog.V(3).Infof("Failed to activate connection %s (may be expected): %v", connID, err)
		}
	}

	return nil
}

// GetInterfaceMTU retrieves the current MTU of an interface
func (n *NetworkManagerBackend) GetInterfaceMTU(interfaceName string) (int, error) {
	return util.GetCurrentMTU(interfaceName)
}

// IsDHCPConfigured checks if DHCP is enabled for an interface using NetworkManager
func (n *NetworkManagerBackend) IsDHCPConfigured(interfaceName string) (bool, error) {
	// Try to find a connection for this interface
	connections, err := n.listConnections()
	if err != nil {
		return false, fmt.Errorf("failed to list connections: %w", err)
	}

	conn, err := n.getDBusConn()
	if err != nil {
		return false, err
	}

	// Search for a connection matching this interface
	for _, connPath := range connections {
		connObj := conn.Object(nmService, connPath)

		var settings map[string]map[string]dbus.Variant
		err := connObj.Call(nmConnIface+".GetSettings", 0).Store(&settings)
		if err != nil {
			continue
		}

		// Check if this connection is for our interface
		if connSettings, ok := settings["connection"]; ok {
			if ifnameVariant, ok := connSettings["interface-name"]; ok {
				if ifname, ok := ifnameVariant.Value().(string); ok && ifname == interfaceName {
					// Found the connection, check IPv4 method
					if ipv4Settings, ok := settings["ipv4"]; ok {
						if methodVariant, ok := ipv4Settings["method"]; ok {
							if method, ok := methodVariant.Value().(string); ok {
								return method == "auto", nil
							}
						}
					}
				}
			}
		}
	}

	// No NetworkManager connection found, check if interface has DHCP address
	// This is a fallback for interfaces not managed by NetworkManager
	return false, nil
}

// Helper functions

// connectionExists checks if a NetworkManager connection profile exists
func (n *NetworkManagerBackend) connectionExists(connName string) (bool, error) {
	_, err := n.getConnectionByName(connName)
	return err == nil, nil
}

// getConnectionByName finds a connection by its ID (name)
func (n *NetworkManagerBackend) getConnectionByName(connName string) (dbus.ObjectPath, error) {
	connections, err := n.listConnections()
	if err != nil {
		return "", err
	}

	conn, err := n.getDBusConn()
	if err != nil {
		return "", err
	}

	for _, connPath := range connections {
		connObj := conn.Object(nmService, connPath)

		var settings map[string]map[string]dbus.Variant
		err := connObj.Call(nmConnIface+".GetSettings", 0).Store(&settings)
		if err != nil {
			continue
		}

		if idVariant, ok := settings["connection"]["id"]; ok {
			if id, ok := idVariant.Value().(string); ok && id == connName {
				return connPath, nil
			}
		}
	}

	return "", fmt.Errorf("connection %s not found", connName)
}

// listConnections lists all NetworkManager connections
func (n *NetworkManagerBackend) listConnections() ([]dbus.ObjectPath, error) {
	conn, err := n.getDBusConn()
	if err != nil {
		return nil, err
	}

	obj := conn.Object(nmService, nmSettingsPath)
	var connections []dbus.ObjectPath
	err = obj.Call(nmSettingsIface+".ListConnections", 0).Store(&connections)
	if err != nil {
		return nil, fmt.Errorf("failed to list connections: %w", err)
	}

	return connections, nil
}

// createConnection creates a new connection profile
func (n *NetworkManagerBackend) createConnection(connName, connType, interfaceName string) error {
	conn, err := n.getDBusConn()
	if err != nil {
		return err
	}

	obj := conn.Object(nmService, nmSettingsPath)
	settings := makeConnectionSettings(connName, connType, interfaceName)

	var connPath dbus.ObjectPath
	err = obj.Call(nmSettingsIface+".AddConnection", 0, settings).Store(&connPath)
	if err != nil {
		return fmt.Errorf("failed to add connection: %w", err)
	}

	klog.V(3).Infof("Created connection %s at %s", connName, connPath)
	return nil
}

// getConnectionSettings retrieves settings for a connection
func (n *NetworkManagerBackend) getConnectionSettings(connPath dbus.ObjectPath) (map[string]map[string]dbus.Variant, error) {
	conn, err := n.getDBusConn()
	if err != nil {
		return nil, err
	}

	obj := conn.Object(nmService, connPath)
	var settings map[string]map[string]dbus.Variant
	err = obj.Call(nmConnIface+".GetSettings", 0).Store(&settings)
	if err != nil {
		return nil, fmt.Errorf("failed to get settings: %w", err)
	}

	return settings, nil
}

// updateConnection updates a connection with new settings
func (n *NetworkManagerBackend) updateConnection(connPath dbus.ObjectPath, settings map[string]map[string]dbus.Variant) error {
	conn, err := n.getDBusConn()
	if err != nil {
		return err
	}

	obj := conn.Object(nmService, connPath)
	err = obj.Call(nmConnIface+".Update", 0, settings).Err
	if err != nil {
		return fmt.Errorf("failed to update connection: %w", err)
	}

	return nil
}

// activateConnection activates a connection
func (n *NetworkManagerBackend) activateConnection(connPath dbus.ObjectPath) error {
	conn, err := n.getDBusConn()
	if err != nil {
		return err
	}

	obj := conn.Object(nmService, nmPath)

	// ActivateConnection(connection, device, specific_object)
	// device and specific_object can be "/" for auto-selection
	var activeConnPath dbus.ObjectPath
	err = obj.Call(nmIface+".ActivateConnection", 0, connPath, dbus.ObjectPath("/"), dbus.ObjectPath("/")).Store(&activeConnPath)
	if err != nil {
		return fmt.Errorf("failed to activate connection: %w", err)
	}

	return nil
}

// checkBridgeMTUChangeNeeded determines if the bridge and its member interfaces need MTU changes
func (n *NetworkManagerBackend) checkBridgeMTUChangeNeeded(bridgeName string, desiredMTU int) (bool, error) {
	// Check current bridge MTU
	currentBridgeMTU, err := util.GetCurrentMTU(bridgeName)
	if err != nil {
		return false, fmt.Errorf("failed to get current bridge MTU: %w", err)
	}

	// If bridge MTU differs, we need to apply changes
	if currentBridgeMTU != desiredMTU {
		klog.Infof("Bridge %s MTU mismatch (current=%d, desired=%d)", bridgeName, currentBridgeMTU, desiredMTU)
		return true, nil
	}

	// Check all member interface MTUs
	memberNames, err := util.GetBridgeMembers(bridgeName)
	if err != nil {
		return false, fmt.Errorf("failed to get bridge members for %s: %w", bridgeName, err)
	}

	// Check if any member interface has different MTU
	for _, memberName := range memberNames {
		currentMTU, err := util.GetCurrentMTU(memberName)
		if err != nil {
			return false, fmt.Errorf("failed to get current MTU for bridge member %s: %w", memberName, err)
		}
		if currentMTU != desiredMTU {
			klog.Infof("Bridge member %s MTU mismatch (current=%d, desired=%d)", memberName, currentMTU, desiredMTU)
			return true, nil
		}
	}

	// No changes needed - all MTUs match desired state
	return false, nil
}
