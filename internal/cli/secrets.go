package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
)

// secretStore holds secrets the user typed into the viewer for `sbx mcp`. Values live only
// in this process's memory and leave it only as input to a sandbox, never in a tool
// result, so they stay out of the agent's context and transcript.
type secretStore struct {
	mu      sync.Mutex
	values  map[string]string
	pending []*secretRequest
	changed chan struct{} // closed and replaced whenever pending changes
}

type secretRequest struct {
	ID          string `json:"id"`
	Sandbox     string `json:"-"`
	Name        string `json:"name"`
	Description string `json:"description"`
	answer      chan *string
}

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

var errSecretDeclined = errors.New("the user declined")

func newSecretStore() *secretStore {
	return &secretStore{values: map[string]string{}, changed: make(chan struct{})}
}

func (s *secretStore) get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	return v, ok
}

// request asks the user, through sandbox's viewer, for a value to store under name. It
// blocks until the user saves or declines, or ctx ends.
func (s *secretStore) request(ctx context.Context, sandbox, name, description string) error {
	if !secretName.MatchString(name) {
		return fmt.Errorf("secret name %q must be 1-64 letters, digits, '.', '_', '@', or '-'", name)
	}
	req := &secretRequest{ID: rand.Text(), Sandbox: sandbox, Name: name, Description: description, answer: make(chan *string, 1)}
	s.update(func() { s.pending = append(s.pending, req) })
	var value *string
	select {
	case <-ctx.Done():
	case value = <-req.answer:
	}
	// Once the request leaves pending, answer refuses it, so an answer that raced ctx
	// is in the channel now or was never accepted.
	s.update(func() { s.pending = slices.DeleteFunc(s.pending, func(r *secretRequest) bool { return r == req }) })
	if value == nil {
		select {
		case value = <-req.answer:
		default:
		}
	}
	switch {
	case value != nil:
		s.mu.Lock()
		s.values[name] = *value
		s.mu.Unlock()
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return errSecretDeclined
	}
}

// answer resolves an open request; a nil value declines it.
func (s *secretStore) answer(id string, value *string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.pending {
		if r.ID == id {
			select {
			case r.answer <- value:
				return true
			default:
				return false
			}
		}
	}
	return false
}

// pendingFor lists the open requests for sandbox, and a channel closed when that list may
// have changed.
func (s *secretStore) pendingFor(sandbox string) ([]secretRequest, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []secretRequest{}
	for _, r := range s.pending {
		if r.Sandbox == sandbox {
			list = append(list, *r)
		}
	}
	return list, s.changed
}

func (s *secretStore) update(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	close(s.changed)
	s.changed = make(chan struct{})
}
