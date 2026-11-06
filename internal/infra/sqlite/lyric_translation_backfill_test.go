package sqlite

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestSplitLegacyBilingual(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantPrimary string
		wantTrans   string
	}{
		{
			name:        "caret separator",
			content:     "[00:01.00]晴天 ^ Clear sky",
			wantPrimary: "[00:01.00]晴天",
			wantTrans:   "Clear sky",
		},
		{
			name:        "slash separator",
			content:     "[00:01.00]晴天 / Clear sky",
			wantPrimary: "[00:01.00]晴天",
			wantTrans:   "Clear sky",
		},
		{
			name:        "line count preserved",
			content:     "[00:01.00]晴天 ^ Clear sky\n[00:02.00]故事",
			wantPrimary: "[00:01.00]晴天\n[00:02.00]故事",
			wantTrans:   "Clear sky\n",
		},
		{
			name:        "a slash without whitespace is not bilingual",
			content:     "[00:01.00]AC/DC",
			wantPrimary: "[00:01.00]AC/DC",
			wantTrans:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary, translation := splitLegacyBilingual(tc.content)
			if primary != tc.wantPrimary || translation != tc.wantTrans {
				t.Fatalf("splitLegacyBilingual(%q) = %q / %q, want %q / %q",
					tc.content, primary, translation, tc.wantPrimary, tc.wantTrans)
			}
		})
	}
}

func TestBackfillLyricTranslations(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := NewDB(":memory:", logger)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	for _, id := range []string{"t1", "t2"} {
		if _, err := db.Exec(`
			INSERT INTO tracks (id, path, title, sort_title, mtime)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`, id, "/music/"+id+".mp3", id, id); err != nil {
			t.Fatalf("insert track %s: %v", id, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO lyrics (track_id, content, source) VALUES ('t1', '[00:01]晴天 ^ Clear sky', 'wy-plugin')`); err != nil {
		t.Fatalf("insert legacy lyric: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO lyrics (track_id, content, source, translation_content)
		VALUES ('t2', '[00:01]故事 ^ Story', 'wy-plugin', '[00:01]Already')`); err != nil {
		t.Fatalf("insert translated lyric: %v", err)
	}

	if err := backfillLyricTranslations(db, logger); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var content, translation string
	if err := db.Get(&content, `SELECT content FROM lyrics WHERE track_id = 't1'`); err != nil {
		t.Fatalf("read t1 content: %v", err)
	}
	if err := db.Get(&translation, `SELECT translation_content FROM lyrics WHERE track_id = 't1'`); err != nil {
		t.Fatalf("read t1 translation: %v", err)
	}
	if content != "[00:01]晴天" || translation != "Clear sky" {
		t.Fatalf("t1 = %q / %q, want the original and the translation split", content, translation)
	}

	// An existing translation is never overwritten, so a second run is a no-op.
	if err := backfillLyricTranslations(db, logger); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if err := db.Get(&content, `SELECT content FROM lyrics WHERE track_id = 't2'`); err != nil {
		t.Fatalf("read t2 content: %v", err)
	}
	if !strings.Contains(content, "Story") || !strings.Contains(content, "[00:01]") {
		t.Fatalf("t2 content = %q, want it untouched", content)
	}
}
