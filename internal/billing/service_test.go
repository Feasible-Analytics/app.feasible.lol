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
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// TestDeleteCustomerRequiresConfiguredCredentials proves self-hosting without
// Stripe is valid only when the account has no retained provider identity.
func TestDeleteCustomerRequiresConfiguredCredentials(t *testing.T) {
	service := &Service{Stripe: stripe.New("")}
	if err := service.DeleteCustomer(context.Background(), ""); err != nil {
		t.Fatalf("empty provider identity should need no external cleanup: %v", err)
	}
	if err := service.DeleteCustomer(context.Background(), "cus_retry"); err == nil || !strings.Contains(err.Error(), "without Stripe credentials") {
		t.Fatalf("unconfigured provider identity error = %v", err)
	}
}
