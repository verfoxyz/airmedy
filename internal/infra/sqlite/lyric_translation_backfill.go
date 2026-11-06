package sqlite

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// legacyBilingualSeparator matches the two concatenation styles the old UI
// split on: "原文 ^ 翻译" and "原文 / 翻译". A slash must be surrounded by
// whitespace so titles such as "AC/DC" are not treated as a bilingual line.
var legacyBilingualSeparator = regexp.MustCompile(`\s*\^\s*|\s+/\s+`)

// backfillLyricTranslations moves the translated half of a legacy concatenated
// lyric into its own column. The rule is the same one the plan fixes:
// the part before the separator is the original, the part after is the
// translation. Rows that already have a translation are left alone, so this is
// safe to run on every startup until no legacy rows remain.
func backfillLyricTranslations(db *DB, logger *slog.Logger) error {
	rows, err := db.Queryx(`
		SELECT track_id, content FROM lyrics
		WHERE translation_content = '' AND (content LIKE '%^%' OR content LIKE '% / %')`)
	if err != nil {
		return fmt.Errorf("select legacy bilingual lyrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type update struct {
		trackID     string
		content     string
		translation string
	}
	var updates []update
	for rows.Next() {
		var trackID, content string
		if err := rows.Scan(&trackID, &content); err != nil {
			return fmt.Errorf("scan legacy bilingual lyric: %w", err)
		}
		primary, translation := splitLegacyBilingual(content)
		if translation == "" {
			continue
		}
		updates = append(updates, update{trackID: trackID, content: primary, translation: translation})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy bilingual lyrics: %w", err)
	}
	if len(updates) == 0 {
		return nil
	}

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin legacy lyric translation backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	for _, u := range updates {
		// The empty-translation guard makes a concurrent write or a second run
		// a no-op instead of overwriting a freshly stored translation.
		if _, err := tx.Exec(`
			UPDATE lyrics SET content = ?, translation_content = ?, updated_at = ?
			WHERE track_id = ? AND translation_content = ''`,
			u.content, u.translation, now, u.trackID); err != nil {
			return fmt.Errorf("backfill legacy lyric translation for %s: %w", u.trackID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy lyric translation backfill: %w", err)
	}
	logger.Info("Backfilled legacy lyric translations", "count", len(updates))
	return nil
}

// splitLegacyBilingual splits every line at its first separator. Lines without
// one keep their text and leave an empty translation slot, preserving the line
// count so a translation without timestamps still aligns by index.
func splitLegacyBilingual(content string) (primary, translation string) {
	lines := strings.Split(content, "\n")
	primaryLines := make([]string, len(lines))
	translationLines := make([]string, len(lines))
	found := false
	for i, line := range lines {
		loc := legacyBilingualSeparator.FindStringIndex(line)
		if loc == nil {
			primaryLines[i] = line
			continue
		}
		primaryLines[i] = strings.TrimSpace(line[:loc[0]])
		translationLines[i] = strings.TrimSpace(line[loc[1]:])
		if translationLines[i] != "" {
			found = true
		}
	}
	if !found {
		return content, ""
	}
	return strings.Join(primaryLines, "\n"), strings.Join(translationLines, "\n")
}
