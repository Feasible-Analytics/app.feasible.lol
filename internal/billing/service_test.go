//
// service_test.go
// Payment-provider adapter behavior at durable deletion boundaries.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// TestDeleteCustomerDoesNotCallMissingConfigurationSuccess ensures a durable
// provider identity remains pending when credentials are unavailable. An empty
// identity is the only configuration-independent no-op.
func TestDeleteCustomerDoesNotCallMissingConfigurationSuccess(t *testing.T) {
	service := &Service{Stripe: stripe.New("")}
	if err := service.DeleteCustomer(context.Background(), "cus_still_live"); err == nil {
		t.Fatal("an unconfigured provider reported a nonempty customer deletion as successful")
	}
	if err := service.DeleteCustomer(context.Background(), ""); err != nil {
		t.Fatalf("an empty provider identity was not a no-op: %v", err)
	}
}
