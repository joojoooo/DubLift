package dublift

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Players retain stream results after navigating back or handing off to VLC.
// A small encrypted ticket lets their URL recreate a discarded session without
// keeping old result lists, credentials or media caches resident on the server.
type playbackTicket struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Content    string          `json:"content"`
	Stream     Stream          `json:"stream"`
	Order      int             `json:"order"`
	Expires    int64           `json:"expires"`
	Native     *nativePlayback `json:"native,omitempty"`
	SourceID   string          `json:"sourceID,omitempty"`
	SourceName string          `json:"sourceName,omitempty"`
}

func openPlaybackKey(configPath string) (cipher.AEAD, error) {
	path := filepath.Join(filepath.Dir(configPath), "playback.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		f, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(createErr, os.ErrExist) {
			b, err = os.ReadFile(path)
		} else if createErr != nil {
			return nil, createErr
		} else {
			_, err = f.Write(b)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) sealPlayback(v *Session) string {
	b, err := json.Marshal(playbackTicket{ID: v.ID, Type: v.Content.Type, Content: v.Content.ID, Stream: v.stream, Order: v.Order, Expires: time.Now().Add(time.Hour).Unix(), Native: v.native, SourceID: v.SourceID, SourceName: v.SourceName})
	if err != nil || len(b) > 16<<10 {
		return ""
	}
	nonce := make([]byte, s.playbackKey.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(s.playbackKey.Seal(nonce, nonce, b, []byte("DubLift playback v1")))
}

func (s *Server) readPlayback(id, encoded string) (playbackTicket, error) {
	var out playbackTicket
	invalid := errors.New("stream link expired or invalid; request fresh streams")
	if len(encoded) > 24<<10 {
		return out, invalid
	}
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	n := s.playbackKey.NonceSize()
	if err != nil || len(b) < n {
		return out, invalid
	}
	plain, err := s.playbackKey.Open(nil, b[:n], b[n:], []byte("DubLift playback v1"))
	if err != nil || json.Unmarshal(plain, &out) != nil || out.ID != id || out.Expires <= time.Now().Unix() {
		return out, invalid
	}
	return out, nil
}

func (s *Server) playbackURL(r *http.Request, v *Session) string {
	u := s.base(r) + "/media/" + v.ID + "/master.m3u8"
	if v.ticket != "" {
		u += "?resume=" + v.ticket
	}
	return u
}

func (s *Server) restorePlayback(id, encoded string) (*Session, error) {
	ticket, err := s.readPlayback(id, encoded)
	if err != nil {
		return nil, err
	}
	c, err := ParseContent(ticket.Type, ticket.Content)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.sessions[id]; v != nil {
		return v, nil
	}
	v := s.newSessionIDLocked(c, ticket.Stream, id)
	v.Order, v.ticket, v.native = ticket.Order, encoded, ticket.Native
	v.SourceID, v.SourceName = ticket.SourceID, ticket.SourceName
	v.ContentName = fallbackContentName(c, ticket.Stream)
	return v, nil
}
