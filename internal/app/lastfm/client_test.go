package lastfm

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterSpacesRequests(t *testing.T) {
	limiter := newRateLimiter(3, 250*time.Millisecond)
	now := time.Unix(0, 0)
	limiter.now = func() time.Time { return now }

	var delays []time.Duration
	limiter.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	for index := 0; index < 5; index++ {
		release, err := limiter.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", index, err)
		}
		release()
	}

	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 750 * time.Millisecond, time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("delay %d = %s, want %s", index, delays[index], want[index])
		}
	}
}

func TestRateLimiterCapsConcurrency(t *testing.T) {
	limiter := newRateLimiter(3, 0)

	var active, maxActive int32
	var wg sync.WaitGroup
	for index := 0; index < 12; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := limiter.acquire(context.Background())
			if err != nil {
				return
			}
			current := atomic.AddInt32(&active, 1)
			for {
				peak := atomic.LoadInt32(&maxActive)
				if current <= peak || atomic.CompareAndSwapInt32(&maxActive, peak, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			release()
		}()
	}
	wg.Wait()

	if peak := atomic.LoadInt32(&maxActive); peak > 3 {
		t.Fatalf("peak concurrency = %d, want at most 3", peak)
	}
}

func TestFlexListAcceptsSingleObject(t *testing.T) {
	body := []byte(`{"name":"Song","artist":{"#text":"Artist"},"album":{"#text":"Album"},` +
		`"duration":"180","playcount":"7","image":[{"#text":"","size":"small"},` +
		`{"#text":"https://img/large.jpg","size":"extralarge"}]}`)

	var list flexList[lastFmTrackWire]
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d, want the single object wrapped in a list", len(list))
	}

	got := list[0].toTrack()
	if got.Artist != "Artist" || got.Title != "Song" || got.Album != "Album" {
		t.Fatalf("track = %+v", got)
	}
	if got.Duration != 180 || got.PlayCount != 7 {
		t.Fatalf("duration/playcount = %d/%d, want 180/7", got.Duration, got.PlayCount)
	}
	if got.ImageURL != "https://img/large.jpg" {
		t.Fatalf("image = %q, want the largest rendition", got.ImageURL)
	}
}

func TestFlexListAcceptsArray(t *testing.T) {
	var list flexList[lastFmTrackWire]
	if err := json.Unmarshal([]byte(`[{"name":"One"},{"name":"Two"}]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
}

func TestWeeklyChartListDecodesStringBounds(t *testing.T) {
	body := []byte(`{"weeklychartlist":{"chart":[` +
		`{"from":"1704067200","to":"1704672000","#text":"..."},` +
		`{"from":"1704672000","to":"1705276800"}]}}`)
	var response struct {
		WeeklyChartList struct {
			Chart flexList[lastFmWeekWire] `json:"chart"`
		} `json:"weeklychartlist"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	weeks := response.WeeklyChartList.Chart
	if len(weeks) != 2 {
		t.Fatalf("weeks = %d, want 2", len(weeks))
	}
	if int64(weeks[0].From) != 1704067200 || int64(weeks[0].To) != 1704672000 {
		t.Fatalf("first week = %d-%d", weeks[0].From, weeks[0].To)
	}
}

func TestWeeklyTrackChartKeepsPlaycountAndCover(t *testing.T) {
	body := []byte(`{"weeklytrackchart":{"track":[` +
		`{"name":"Song","artist":{"#text":"Artist"},"playcount":"42",` +
		`"image":[{"#text":"https://img/large.jpg","size":"extralarge"}]}]}}`)
	var response struct {
		WeeklyTrackChart struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"weeklytrackchart"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tracks := parseTrackList(response.WeeklyTrackChart.Track)
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(tracks))
	}
	if tracks[0].PlayCount != 42 || tracks[0].ImageURL != "https://img/large.jpg" {
		t.Fatalf("track = %+v, want playcount and cover", tracks[0])
	}
}

func TestLargestImageSkipsEmptyRenditions(t *testing.T) {
	images := []lastFmImage{
		{Text: "", Size: "small"},
		{Text: "https://img/medium.jpg", Size: "medium"},
		{Text: "", Size: "extralarge"},
	}
	if got := largestImage(images); got != "https://img/medium.jpg" {
		t.Fatalf("largestImage = %q, want the last non-empty rendition", got)
	}
	if got := largestImage(nil); got != "" {
		t.Fatalf("largestImage(nil) = %q, want empty", got)
	}
}

func TestUsableImageDropsTheLastFmPlaceholder(t *testing.T) {
	placeholder := "https://lastfm-img.freetls.fastly.net/i/u/64s/" + lastFmPlaceholderImageMarker + ".png"
	if got := usableImage(placeholder); got != "" {
		t.Fatalf("usableImage(placeholder) = %q, want empty", got)
	}
	if got := usableImage("  "); got != "" {
		t.Fatalf("usableImage(blank) = %q, want empty", got)
	}
	if got := usableImage("https://img/artist.jpg"); got != "https://img/artist.jpg" {
		t.Fatalf("usableImage(real) = %q, want the URL", got)
	}
}

func TestParseTagListDropsEmptyNames(t *testing.T) {
	tags := parseTagList(flexList[lastFmTagWire]{
		{Name: " rock ", Count: 10},
		{Name: "  "},
		{Name: "pop", Count: 3},
	})
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want two usable entries", tags)
	}
	if tags[0].Name != "rock" || tags[0].Count != 10 {
		t.Fatalf("first tag = %+v", tags[0])
	}
}

// TestParseTopTracksArtistNameShape covers the real user.getTopTracks response:
// its artist object uses "name", not the recent-tracks "#text".
func TestParseTopTracksArtistNameShape(t *testing.T) {
	body := []byte(`{"toptracks":{"track":[` +
		`{"name":"新·与天齐","artist":{"name":"指尖笑","mbid":"69a5aa40"},"image":[],"duration":"231"},` +
		`{"name":"Grin","artist":{"name":"Hyun","mbid":""},"image":[],"duration":"180"}` +
		`],"@attr":{"page":"1"}}}`)

	var response struct {
		TopTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"toptracks"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tracks := parseTrackList(response.TopTracks.Track)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want both top tracks", len(tracks))
	}
	if tracks[0].Artist != "指尖笑" || tracks[0].Title != "新·与天齐" {
		t.Fatalf("first track = %+v, want the artist from the name field", tracks[0])
	}
}

// TestParseRecentTracksArtistTextShape keeps the other accepted shape working.
func TestParseRecentTracksArtistTextShape(t *testing.T) {
	body := []byte(`{"recenttracks":{"track":[` +
		`{"name":"Song","artist":{"#text":"Artist","mbid":""},"image":[],"duration":"200"}` +
		`]}}`)

	var response struct {
		RecentTracks struct {
			Track flexList[lastFmTrackWire] `json:"track"`
		} `json:"recenttracks"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tracks := parseTrackList(response.RecentTracks.Track)
	if len(tracks) != 1 || tracks[0].Artist != "Artist" {
		t.Fatalf("tracks = %+v, want the #text artist", tracks)
	}
}

func TestParseTrackArtistNameWinsOverText(t *testing.T) {
	body := []byte(`{"name":"Song","artist":{"name":"Preferred","#text":"Fallback"},"duration":"1"}`)

	var wire lastFmTrackWire
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := wire.toTrack().Artist; got != "Preferred" {
		t.Fatalf("artist = %q, want the name field", got)
	}
}

func TestParseTrackDropsMissingArtist(t *testing.T) {
	tracks := parseTrackList(flexList[lastFmTrackWire]{
		{Name: "No Artist"},
		{Name: "Valid", Artist: lastFmArtistRef{Name: "Artist"}},
	})
	if len(tracks) != 1 || tracks[0].Title != "Valid" {
		t.Fatalf("tracks = %+v, want the artist-less row filtered", tracks)
	}
}
