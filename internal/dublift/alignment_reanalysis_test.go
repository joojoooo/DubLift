package dublift

import (
	"path/filepath"
	"testing"
	"time"
)

func agreedAlignment() Alignment {
	return Alignment{AutoExpected: 3, AutoComplete: true, AutoSamples: []Anchor{{SourceTime: 30, Offset: .1, Confidence: .9}, {SourceTime: 400, Offset: .09, Confidence: .95}, {SourceTime: 800, Offset: .11, Confidence: .92}}, Offset: .1, Confidence: .9}
}

func TestFailedReanalysisClearsStaleAutomaticOffset(t *testing.T) {
	cfg, err := OpenConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	v := server.newSession(Content{Type: "movie", ID: "tmdb:603"}, Stream{URL: "https://example.test/unavailable.m3u8"})
	// Empty media segments make extraction fail before any network or process.
	v.video = &Asset{ID: "unavailable", HLS: &HLS{Duration: 100}}
	tk := &Track{ID: "english", Asset: v.video, Selector: "0:a:0"}
	v.sourceEnglish, v.vixEnglish = tk, tk
	server.Alignments.Update(v.Key, func(a *Alignment) {
		*a = agreedAlignment()
		a.Offset = 2
	})
	if !server.startAlignment(v, nil) {
		t.Fatal("analysis was not started")
	}
	v.mu.Lock()
	done := v.alignDone
	v.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed analysis did not finish")
	}
	a := server.Alignments.Get(v.Key)
	if a.Offset != 0 || a.Confidence != 0 || !a.AutoComplete || len(a.AutoSamples) != 0 || a.At(10, .68) != 0 {
		t.Fatalf("failed reanalysis retained a stale result: %+v", a)
	}
}
