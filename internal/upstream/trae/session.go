// ═══ 更新日志 ═══
// 2026-11-15：保留会话隔离指纹，另派生每轮独立的商业账单 ID，避免累积账单串轮。
package trae

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"workbuddy2api/internal/auth"
)

// SessionIdentity contains authenticated caller identity (never an API secret)
// and the client's conversation identity. Content hashes are not session IDs:
// two independent conversations can have exactly the same messages.
type SessionIdentity struct {
	CallerID       string
	ConversationID string
}

type sessionIdentityKey struct{}
type sessionIdentity struct {
	SessionIdentity
	nonce string
}

func withSessionIdentity(ctx context.Context, identities []SessionIdentity) context.Context {
	identity := sessionIdentity{nonce: rand.Text()}
	if len(identities) > 0 {
		identity.SessionIdentity = identities[0]
	}
	return context.WithValue(ctx, sessionIdentityKey{}, identity)
}

func sessionID(ctx context.Context, a *auth.Auth) string {
	identity, ok := ctx.Value(sessionIdentityKey{}).(sessionIdentity)
	if !ok {
		identity.nonce = rand.Text()
	}
	conversation, source := identity.ConversationID, "conversation"
	if conversation == "" || identity.CallerID == "" {
		// No reliable conversation identity: isolate this request. Do not merge
		// identical prompts or promise cache affinity across anonymous turns.
		conversation = identity.nonce
		source = "request"
	}
	framed, _ := json.Marshal([]string{"trae-session-v1", identity.CallerID, a.Realm(), a.UID, source, conversation})
	sum := sha256.Sum256(framed)
	return hex.EncodeToString(sum[:])
}

// billingSessionID derives a per-request commercial billing key. The upstream
// session_id groups bills, so the stable conversation fingerprint must never
// be sent as that key across turns. requestID must identify this one request;
// the caller sends the resulting ID and uses the same ID when fetching its bill.
func billingSessionID(ctx context.Context, a *auth.Auth, requestID string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(sessionIdentityKey{}).(sessionIdentity); !ok {
		// Direct/internal callers may not have ChatStream's identity context.
		// The unique request ID keeps this fallback deterministic for this call
		// without merging separate anonymous requests.
		ctx = context.WithValue(ctx, sessionIdentityKey{}, sessionIdentity{nonce: requestID})
	}
	framed, _ := json.Marshal([]string{"trae-billing-session-v1", sessionID(ctx, a), requestID})
	sum := sha256.Sum256(framed)
	return hex.EncodeToString(sum[:])
}
