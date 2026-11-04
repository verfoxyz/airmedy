package audio

/*
#include <stdlib.h>
#include "ffmpeg_analyzer.h"
*/
import "C"

import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"airmedy/internal/domain"
)

// ffmpegDurationProbe implements domain.MediaDurationProbe by reading the
// container/stream header through the vendored ffmpeg build. It never decodes
// audio, so a probe is milliseconds even for a long track.
type ffmpegDurationProbe struct{}

// NewDurationProbe returns the in-process ffmpeg-backed duration probe.
func NewDurationProbe() domain.MediaDurationProbe {
	return &ffmpegDurationProbe{}
}

func (p *ffmpegDurationProbe) Probe(ctx context.Context, path string) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	// The cancel flag lives in C memory for the same reason as the analyzer: the
	// watcher goroutine and the C call share it without violating cgo pointer
	// rules.
	cancel := (*C.int)(C.malloc(C.size_t(unsafe.Sizeof(C.int(0)))))
	*cancel = 0
	defer C.free(unsafe.Pointer(cancel))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			*cancel = 1
		case <-stop:
		}
	}()

	var seconds C.double
	rc := int(C.ffmpeg_probe_duration(cPath, &seconds, cancel))
	if rc != 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("probe duration %s: %w", path, probeError(rc))
	}
	return time.Duration(float64(time.Second) * float64(seconds)), nil
}

func probeError(rc int) error {
	switch rc {
	case int(C.FFA_PROBE_ERR_OPEN):
		return fmt.Errorf("could not open the file")
	case int(C.FFA_PROBE_ERR_NO_AUDIO):
		return fmt.Errorf("no audio stream")
	case int(C.FFA_PROBE_ERR_UNKNOWN):
		return fmt.Errorf("the header carries no usable duration")
	case int(C.FFA_PROBE_ERR_CANCELLED):
		return context.Canceled
	default:
		return fmt.Errorf("unknown probe error (%d)", rc)
	}
}
