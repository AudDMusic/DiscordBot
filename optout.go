package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const privacyPolicyURL = "https://audd.io/privacy/discord-bot/"

var errOptedOut = errors.New("the author of the message has opted out")

// optOutStore remembers the users who asked the bot not to process their messages and voice.
// Only the user IDs are kept, in a small JSON file.
type optOutStore struct {
	mu   sync.RWMutex
	path string
	ids  map[string]bool
}

func loadOptOuts(path string) (*optOutStore, error) {
	store := &optOutStore{path: path, ids: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	for _, id := range ids {
		store.ids[id] = true
	}
	return store, nil
}

func (s *optOutStore) Has(userID string) bool {
	if s == nil || userID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ids[userID]
}

// Set records the choice of the user and saves the list. The change is only kept if it was saved.
func (s *optOutStore) Set(userID string, optedOut bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[userID] == optedOut {
		return nil
	}
	ids := make([]string, 0, len(s.ids)+1)
	for id := range s.ids {
		if id != userID {
			ids = append(ids, id)
		}
	}
	if optedOut {
		ids = append(ids, userID)
	}
	sort.Strings(ids)
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	// Write to a temporary file and rename it, so a crash can't leave a half-written list.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".optouts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		return err
	}
	if optedOut {
		s.ids[userID] = true
	} else {
		delete(s.ids, userID)
	}
	return nil
}

const (
	replyOptedOut = "Done. I won't read your messages or listen to you in voice channels anymore, " +
		"and other people can't ask me to identify music from your messages. " +
		"To remember this, I keep your user ID. To undo it, use /privacy again and pick opt-in."
	replyOptedIn        = "Done. You're opted back in, and your user ID is removed from the opt-out list."
	replyAuthorOptedOut = "The author of that message has asked me not to process their messages."
)
