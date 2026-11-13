package sendqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RedeliveryDecision is immutable for a request ID. Current record state is
// returned separately: a pending decision can subsequently have landed.
type RedeliveryDecision struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

type redeliveryRequest struct {
	SendID   string             `json:"send_id"`
	Decision RedeliveryDecision `json:"decision"`
}

// ValidRedeliveryRequestID bounds untrusted keys. They are hashed, never paths.
func ValidRedeliveryRequestID(key string) bool {
	return len(key) > 0 && len(key) <= 256 && strings.TrimSpace(key) == key
}

func redeliveryRequestPath(dir, key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "redeliver-keys", hex.EncodeToString(h[:])+".json")
}

// ReadRedeliveryRequest reads an existing binding without creating a lock or
// file. Atomic publication makes this safe for --check-only as well as for a
// caller already holding the request lock. An absent binding returns false.
func ReadRedeliveryRequest(dir, id, key string) (RedeliveryDecision, bool, error) {
	if !ValidRedeliveryRequestID(key) || id == "" {
		return RedeliveryDecision{}, false, errors.New("invalid redelivery request")
	}
	b, err := os.ReadFile(redeliveryRequestPath(dir, key))
	if os.IsNotExist(err) {
		return RedeliveryDecision{}, false, nil
	}
	if err != nil {
		return RedeliveryDecision{}, false, err
	}
	var prior redeliveryRequest
	if err := json.Unmarshal(b, &prior); err != nil || prior.SendID == "" || prior.Decision.Outcome == "" {
		return RedeliveryDecision{}, false, errors.New("cannot read redelivery request binding")
	}
	if prior.SendID != id {
		return RedeliveryDecision{Outcome: "conflict", Reason: "request_id_bound_to_another_send"}, true, nil
	}
	return prior.Decision, true, nil
}

// RedeliverOnce binds key to id before fn can authorize any work. Replays
// never call fn, including after a crash. A crash between registration and
// completion leaves pending/request_registered; its current snapshot is the
// authority on progress. This deliberately sacrifices liveness in that gap
// rather than authorizing another attempt. Bindings outlive pruned records and
// live outside Record so an older worker cannot erase them when it saves.
func RedeliverOnce(dir, id, key string, fn func() (RedeliveryDecision, error)) (RedeliveryDecision, bool, error) {
	if !ValidRedeliveryRequestID(key) || id == "" {
		return RedeliveryDecision{}, false, errors.New("request-id must be 1 to 256 bytes without surrounding whitespace")
	}
	path := redeliveryRequestPath(dir, key)
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return RedeliveryDecision{}, false, err
	}
	defer unlock()
	if prior, found, err := ReadRedeliveryRequest(dir, id, key); err != nil || found {
		return prior, found, err
	}
	save := func(d RedeliveryDecision) error {
		b, err := json.Marshal(redeliveryRequest{SendID: id, Decision: d})
		if err != nil {
			return err
		}
		return writeFileAtomic(path, b)
	}
	if err := save(RedeliveryDecision{Outcome: "pending", Reason: "request_registered"}); err != nil {
		return RedeliveryDecision{}, false, err
	}
	d, err := fn()
	if err != nil {
		return d, false, err
	}
	return d, false, save(d)
}

// ListForProof fails closed on unreadable records: silently skipping one can
// hide a row claim or the next send's offset. Normal queue listing is tolerant.
func ListForProof(dir string) ([]*Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || !validID(id) {
			continue
		}
		r, err := Load(dir, id)
		if errors.Is(err, ErrUnknown) {
			continue // pruned concurrently
		}
		if err != nil {
			return nil, fmt.Errorf("reading send proof %s: %w", id, err)
		}
		out = append(out, r)
	}
	return out, nil
}
