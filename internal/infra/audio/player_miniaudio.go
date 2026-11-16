//go:build windows || linux

package audio

/*
#cgo CFLAGS: -I${SRCDIR}/miniaudio -I${SRCDIR} -std=c11


#include "miniaudio_wrapper.h"
#include <stdlib.h>

extern void goMiniAudioTrackEnd(void* userdata);
*/
import "C"
import (
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"airmedy/internal/domain"
)

var (
	miniAudioEndCallback func()
	miniAudioEndMu       sync.Mutex
)

//export goMiniAudioTrackEnd
func goMiniAudioTrackEnd(_ unsafe.Pointer) {
	miniAudioEndMu.Lock()
	cb := miniAudioEndCallback
	miniAudioEndMu.Unlock()
	if cb != nil {
		// MUST run in a goroutine. Miniaudio documentation warns not to
		// uninitialize or change sound state from within the callback thread.
		// Since HandleTrackEnd calls Load() -> ma_sound_uninit(), it would deadlock.
		go cb()
	}
}

type MiniAudioPlayer struct {
	logger *slog.Logger
	ptr    unsafe.Pointer // *MaPlayer
	status domain.PlayerStatus
	np     nowPlayingBackend // OS media integration (MPRIS on Linux, SMTC on Windows); may be nil
}

func NewPlayer(logger *slog.Logger) domain.AudioPlayer {
	p := C.ma_player_create()
	if p == nil {
		logger.Error("MiniAudioPlayer: failed to initialise audio engine")
	}
	return &MiniAudioPlayer{
		logger: logger,
		ptr:    unsafe.Pointer(p),
		status: domain.PlayerStatus{
			PlaybackState: domain.PlaybackStateStopped,
			Volume:        1.0,
		},
		np: newNowPlayingBackend(logger),
	}
}

// Close releases native and OS-integration resources.
func (p *MiniAudioPlayer) Close() {
	if p.np != nil {
		p.np.close()
	}
	if p.ptr == nil {
		return
	}
	// Destroy cancels a decoder blocked in the cache AVIO read, then tears down
	// the sound, EQ nodes and engine. Without it an exit during a failed stream
	// waits for that read's 30s timeout.
	C.ma_player_destroy((*C.MaPlayer)(p.ptr))
	p.ptr = nil
}

func (p *MiniAudioPlayer) Play() error {
	if rc := C.ma_player_play((*C.MaPlayer)(p.ptr)); rc != 0 {
		return fmt.Errorf("ma_player_play failed: %d", rc)
	}
	p.status.PlaybackState = domain.PlaybackStatePlaying
	if p.np != nil {
		p.np.setPlaybackState(domain.PlaybackStatePlaying)
	}
	return nil
}

func (p *MiniAudioPlayer) Pause() error {
	if rc := C.ma_player_pause((*C.MaPlayer)(p.ptr)); rc != 0 {
		return fmt.Errorf("ma_player_pause failed: %d", rc)
	}
	p.status.PlaybackState = domain.PlaybackStatePaused
	if p.np != nil {
		p.np.setPlaybackState(domain.PlaybackStatePaused)
	}
	return nil
}

func (p *MiniAudioPlayer) Stop() error {
	if rc := C.ma_player_stop((*C.MaPlayer)(p.ptr)); rc != 0 {
		return fmt.Errorf("ma_player_stop failed: %d", rc)
	}
	p.status.PlaybackState = domain.PlaybackStateStopped
	p.status.Position = 0
	if p.np != nil {
		p.np.setPlaybackState(domain.PlaybackStateStopped)
	}
	return nil
}

func (p *MiniAudioPlayer) Seek(position float64) error {
	if rc := C.ma_player_seek((*C.MaPlayer)(p.ptr), C.double(position)); rc != 0 {
		return fmt.Errorf("ma_player_seek failed: %d", rc)
	}
	p.status.Position = position
	return nil
}

func (p *MiniAudioPlayer) SetVolume(volume float64) error {
	if rc := C.ma_player_set_volume((*C.MaPlayer)(p.ptr), C.float(volume)); rc != 0 {
		return fmt.Errorf("ma_player_set_volume failed: %d", rc)
	}
	p.status.Volume = volume
	return nil
}

func (p *MiniAudioPlayer) SetMuted(muted bool) error {
	var vol C.float
	if muted {
		vol = 0.0
	} else {
		vol = C.float(p.status.Volume)
	}
	if rc := C.ma_player_set_volume((*C.MaPlayer)(p.ptr), vol); rc != 0 {
		return fmt.Errorf("ma_player_set_volume failed: %d", rc)
	}
	p.status.Muted = muted
	return nil
}

func (p *MiniAudioPlayer) Load(track *domain.TrackDTO) error {
	// Cache mode: the decoder reads the growing cache file through a blocking
	// AVIO, so FFmpeg never performs network I/O or re-probes an HTTP URL.
	if track.StreamCachePath != "" {
		cPath := C.CString(track.StreamCachePath)
		defer C.free(unsafe.Pointer(cPath))
		if rc := C.ma_player_load_cache(
			(*C.MaPlayer)(p.ptr), cPath, C.int64_t(track.StreamTotalBytes),
		); rc != 0 {
			return fmt.Errorf("ma_player_load_cache failed: %d (path: %s)", rc, track.StreamCachePath)
		}
		p.status.TrackID = track.ID
		p.status.Duration = float64(C.ma_player_get_length((*C.MaPlayer)(p.ptr)))
		p.status.Position = 0
		return nil
	}

	cPath := C.CString(track.Path)
	defer C.free(unsafe.Pointer(cPath))

	if rc := C.ma_player_load((*C.MaPlayer)(p.ptr), cPath); rc != 0 {
		return fmt.Errorf("ma_player_load failed: %d (path: %s)", rc, track.Path)
	}

	p.status.TrackID = track.ID
	p.status.Duration = float64(C.ma_player_get_length((*C.MaPlayer)(p.ptr)))
	p.status.Position = 0
	return nil
}

func (p *MiniAudioPlayer) Unload() error {
	if err := p.Stop(); err != nil {
		p.logger.Warn("MiniAudioPlayer: stop during unload", "err", err)
	}
	C.ma_player_unload((*C.MaPlayer)(p.ptr))
	p.status.TrackID = ""
	p.status.Duration = 0
	p.status.Position = 0
	return nil
}

// MarkStreamComplete tells the cache-mode decoder that a download with no
// announced length has finished; its next read at the file edge becomes EOF.
func (p *MiniAudioPlayer) MarkStreamComplete() {
	if p.ptr == nil {
		return
	}
	C.ma_player_mark_stream_complete((*C.MaPlayer)(p.ptr))
}

// GetDiagnostics snapshots the decoder/output counters for the seek-silence
// investigation. Cheap enough to call once per seek.
func (p *MiniAudioPlayer) GetDiagnostics() domain.PlayerDiagnostics {
	if p.ptr == nil {
		return domain.PlayerDiagnostics{}
	}
	var diag C.MaPlayerDiag
	C.ma_player_get_diag((*C.MaPlayer)(p.ptr), &diag)
	return domain.PlayerDiagnostics{
		Reads:         int64(diag.reads),
		Frames:        int64(diag.frames),
		EmptyReads:    int64(diag.empty_reads),
		AVIOWaits:     int64(diag.avio_waits),
		AVIOWaitMS:    int64(diag.avio_wait_ms),
		FilePos:       int64(diag.file_pos),
		FileSize:      int64(diag.file_size),
		ExpectedSize:  int64(diag.expected_size),
		CursorSeconds: float64(diag.cursor_seconds),
		SoundVolume:   float64(diag.sound_volume),
		EngineVolume:  float64(diag.engine_volume),
		Loaded:        diag.loaded != 0,
		SeekRequested: float64(diag.seek_requested),
		SeekReceived:  int64(diag.seek_received),
		SeekTotal:     int64(diag.seek_total),
		SeekOK:        diag.seek_ok != 0,
	}
}

func (p *MiniAudioPlayer) GetStatus() domain.PlayerStatus {
	p.status.Position = float64(C.ma_player_get_cursor((*C.MaPlayer)(p.ptr)))
	return p.status
}

func (p *MiniAudioPlayer) OnTrackEnd(callback func()) {
	miniAudioEndMu.Lock()
	miniAudioEndCallback = callback
	miniAudioEndMu.Unlock()
	C.ma_player_set_end_callback(
		(*C.MaPlayer)(p.ptr),
		C.MaEndCallback(C.goMiniAudioTrackEnd),
		nil,
	)
}

func (p *MiniAudioPlayer) SetEQBand(index int, frequency, gain, bandwidth float64) error {
	if rc := C.ma_player_set_eq_band((*C.MaPlayer)(p.ptr), C.int(index), C.float(frequency), C.float(gain), C.float(bandwidth)); rc != 0 {
		return fmt.Errorf("ma_player_set_eq_band failed: %d", rc)
	}
	return nil
}

func (p *MiniAudioPlayer) SetEQEnabled(enabled bool) error {
	e := 0
	if enabled {
		e = 1
	}
	if rc := C.ma_player_set_eq_enabled((*C.MaPlayer)(p.ptr), C.int(e)); rc != 0 {
		return fmt.Errorf("ma_player_set_eq_enabled failed: %d", rc)
	}
	return nil
}

// --- EQPreampController ---

func (p *MiniAudioPlayer) SetEQPreamp(db float64) error {
	if rc := C.ma_player_set_eq_preamp((*C.MaPlayer)(p.ptr), C.float(db)); rc != 0 {
		return fmt.Errorf("ma_player_set_eq_preamp failed: %d", rc)
	}
	return nil
}

// --- StereoWidthController ---

func (p *MiniAudioPlayer) SetStereoWidth(widthPercent float64) error {
	if rc := C.ma_player_set_stereo_width((*C.MaPlayer)(p.ptr), C.float(widthPercent)); rc != 0 {
		return fmt.Errorf("ma_player_set_stereo_width failed: %d", rc)
	}
	return nil
}

// --- NormalizationController ---

func (p *MiniAudioPlayer) SetPreampGain(db float64) error {
	if rc := C.ma_player_set_preamp_gain((*C.MaPlayer)(p.ptr), C.float(db)); rc != 0 {
		return fmt.Errorf("ma_player_set_preamp_gain failed: %d", rc)
	}
	return nil
}

// --- GaplessPlayer ---

func (p *MiniAudioPlayer) EnqueueNext(track *domain.TrackDTO) error {
	cPath := C.CString(track.Path)
	defer C.free(unsafe.Pointer(cPath))
	if rc := C.ma_player_preload_next((*C.MaPlayer)(p.ptr), cPath); rc != 0 {
		return fmt.Errorf("ma_player_preload_next failed: %d (path: %s)", rc, track.Path)
	}
	return nil
}

func (p *MiniAudioPlayer) StartPreloaded(track *domain.TrackDTO) error {
	if rc := C.ma_player_start_preloaded((*C.MaPlayer)(p.ptr)); rc != 0 {
		return fmt.Errorf("ma_player_start_preloaded failed: %d", rc)
	}
	p.status.TrackID = track.ID
	p.status.Duration = float64(C.ma_player_get_length((*C.MaPlayer)(p.ptr)))
	p.status.Position = 0
	p.status.PlaybackState = domain.PlaybackStatePlaying
	return nil
}

func (p *MiniAudioPlayer) AutoTransitions() bool {
	return false
}

func (p *MiniAudioPlayer) ClearEnqueued() {
	C.ma_player_clear_preloaded((*C.MaPlayer)(p.ptr))
}

// --- CrossfadePlayer ---

// SetCrossfadeDuration is Go-side only: the native layer takes the duration
// per ma_player_begin_crossfade call.
func (p *MiniAudioPlayer) SetCrossfadeDuration(seconds float64) {}

// BeginCrossfadeToPreloaded overlaps the pre-loaded sound with the current
// one under a fade. Must be called from a goroutine, never the audio thread.
func (p *MiniAudioPlayer) BeginCrossfadeToPreloaded(track *domain.TrackDTO, durationSec, preampGainDB float64) error {
	if rc := C.ma_player_begin_crossfade((*C.MaPlayer)(p.ptr), C.double(durationSec), C.float(preampGainDB)); rc != 0 {
		return fmt.Errorf("ma_player_begin_crossfade failed: %d (path: %s)", rc, track.Path)
	}
	p.status.TrackID = track.ID
	p.status.Duration = float64(C.ma_player_get_length((*C.MaPlayer)(p.ptr)))
	p.status.Position = 0
	p.status.PlaybackState = domain.PlaybackStatePlaying
	return nil
}

// FinishCrossfade force-completes an in-progress fade. Must be called from a
// goroutine, never the audio thread (it uninits the outgoing sound).
func (p *MiniAudioPlayer) FinishCrossfade() {
	C.ma_player_finish_crossfade((*C.MaPlayer)(p.ptr))
}
