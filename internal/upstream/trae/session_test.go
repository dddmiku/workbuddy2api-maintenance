package trae

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func billingIdentityContext(caller, conversation string) context.Context {
	return withSessionIdentity(context.Background(), []SessionIdentity{{
		CallerID: caller, ConversationID: conversation,
	}})
}

func TestBillingSessionIDKeepsStableIdentityAndSeparatesTurns(t *testing.T) {
	a := &auth.Auth{UID: "billing-fixture-account-a"}
	ctx := billingIdentityContext("billing-fixture-caller", "billing-fixture-conversation")
	stable := sessionID(ctx, a)
	first := billingSessionID(ctx, a, "billing-fixture-request-1")
	if first != billingSessionID(ctx, a, "billing-fixture-request-1") {
		t.Fatal("the same request changed its billing identity")
	}
	if first == billingSessionID(ctx, a, "billing-fixture-request-2") {
		t.Fatal("separate turns shared a commercial billing identity")
	}
	if stable != sessionID(ctx, a) || first == stable {
		t.Fatal("billing identity replaced or exposed the stable session identity")
	}
	if len(first) != 64 {
		t.Fatal("billing identity must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatal("billing identity must contain only hex characters")
	}
	for _, source := range []string{"billing-fixture-account-a", "billing-fixture-caller", "billing-fixture-conversation", "billing-fixture-request-1"} {
		if strings.Contains(first, source) {
			t.Fatal("billing identity exposed a source identifier")
		}
	}
	// A new request context for an authenticated conversation retains its
	// stable fingerprint; billing uniqueness is deliberately supplied by requestID.
	otherContext := billingIdentityContext("billing-fixture-caller", "billing-fixture-conversation")
	if stable != sessionID(otherContext, a) || first != billingSessionID(otherContext, a, "billing-fixture-request-1") {
		t.Fatal("authenticated conversation identity depended on an incidental nonce")
	}
}

func TestBillingSessionIDSeparatesIdentityBoundaries(t *testing.T) {
	requestID := "billing-fixture-fixed-request"
	a := &auth.Auth{UID: "billing-fixture-account-a"}
	base := billingSessionID(billingIdentityContext("caller-a", "conversation-a"), a, requestID)
	cases := []struct {
		name         string
		caller       string
		conversation string
		account      *auth.Auth
	}{
		{"caller", "caller-b", "conversation-a", a},
		{"conversation", "caller-a", "conversation-b", a},
		{"account", "caller-a", "conversation-a", &auth.Auth{UID: "billing-fixture-account-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if base == billingSessionID(billingIdentityContext(tc.caller, tc.conversation), tc.account, requestID) {
				t.Fatal("billing identity crossed an authenticated identity boundary")
			}
		})
	}
}

func TestBillingSessionIDAnonymousFallbackIsRequestScoped(t *testing.T) {
	a := &auth.Auth{UID: "billing-fixture-anonymous-account"}
	for _, ctx := range []context.Context{nil, context.Background(), billingIdentityContext("", "untrusted-conversation"), billingIdentityContext("caller-a", "")} {
		first := billingSessionID(ctx, a, "anonymous-request-1")
		if first != billingSessionID(ctx, a, "anonymous-request-1") {
			t.Fatal("anonymous request identity was not reproducible within the request")
		}
		if first == billingSessionID(ctx, a, "anonymous-request-2") {
			t.Fatal("independent anonymous requests shared a billing identity")
		}
	}
}
