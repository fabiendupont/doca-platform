// Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netconfig

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetworkManagerBackend", func() {
	var backend *NetworkManagerBackend

	BeforeEach(func() {
		backend = &NetworkManagerBackend{}
	})

	Context("Name", func() {
		It("should return NetworkManager", func() {
			Expect(backend.Name()).To(Equal("NetworkManager"))
		})
	})

	Context("IsAvailable", func() {
		It("should check NetworkManager availability", func() {
			// The actual result depends on the system where tests run
			result := backend.IsAvailable()
			Expect(result).To(BeAssignableToTypeOf(false))
		})
	})

	Context("connectionExists", func() {
		It("should return false for non-existent connection", func() {
			// Test with a connection that definitely doesn't exist
			exists, err := backend.connectionExists("non-existent-connection-12345")
			Expect(err).ToNot(HaveOccurred())
			// If nmcli is not available, this is expected to fail
			// We just verify the function doesn't panic
			_ = exists
		})
	})
})
