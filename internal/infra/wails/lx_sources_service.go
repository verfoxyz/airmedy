package wails

import (
	"context"
	"errors"
	"fmt"

	"airmedy/internal/app/lx"
	"airmedy/internal/domain"
)

// LxSourceDTO is one imported lx custom source as the settings page renders it.
//
// The wire shape is the frontend contract, so field names are explicit. The
// providers come from the plugin's describe answer when the plugin is running
// and are empty otherwise: the script's content, not the database row, decides
// what a source can serve, and only the plugin can read that.
type LxSourceDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// FileName is the stored file's base name (a uuid), shown in diagnostics.
	FileName string `json:"file_name"`
	Enabled  bool   `json:"enabled"`
	Position int    `json:"position"`
	// Providers are the platforms this source registers, as the plugin
	// reported them. Empty means either "not known yet" or "failed to load".
	Providers []string `json:"providers"`
	// Error explains a source the plugin could not load. Empty means healthy.
	Error string `json:"error"`
}

// LxSourcesStateDTO is the whole sub-page in one answer: the list plus the
// plugin's view of it, fetched together so the page never renders half a state.
type LxSourcesStateDTO struct {
	Sources []LxSourceDTO `json:"sources"`
	// Directory is where imported scripts are copied; the page shows it.
	Directory string `json:"directory"`
	// PluginRunning is false when Lx 自定义源 is installed but turned off; the
	// page then shows why providers are unknown instead of pretending they are.
	PluginRunning bool `json:"plugin_running"`
	// PluginError explains why the plugin could not be asked (not installed,
	// not running, describe failed). Empty when the list is authoritative.
	PluginError string `json:"plugin_error"`
}

// LxSourcesService is the Wails binding for the lx source list: translation
// only, no logic (see AGENTS.md §3).
type LxSourcesService struct {
	svc *lx.Service
}

// NewLxSourcesService binds the source list for the frontend.
func NewLxSourcesService(svc *lx.Service) *LxSourcesService {
	return &LxSourcesService{svc: svc}
}

// ListLxSources returns the list, joined with what the plugin reports.
func (s *LxSourcesService) ListLxSources(ctx context.Context) (LxSourcesStateDTO, error) {
	sources, err := s.svc.List(ctx)
	if err != nil {
		return LxSourcesStateDTO{}, err
	}

	state := LxSourcesStateDTO{
		Sources:   make([]LxSourceDTO, 0, len(sources)),
		Directory: s.svc.SourcesDir(),
	}
	for _, source := range sources {
		state.Sources = append(state.Sources, LxSourceDTO{
			ID:        source.ID,
			Name:      source.Name,
			FileName:  baseName(source.FilePath),
			Enabled:   source.Enabled,
			Position:  source.Position,
			Providers: []string{},
		})
	}

	describe, describeErr := s.svc.Describe(ctx)
	if describeErr != nil {
		state.PluginError = translateLxError(describeErr).Error()
		return state, nil
	}
	state.PluginRunning = true

	// Join by id: the plugin is authoritative about what loaded, the row about
	// whether the user wants it. A source the plugin does not mention is left
	// without providers rather than guessed at.
	byID := make(map[string]domain.PluginSourceDescription, len(describe.Sources))
	for _, reported := range describe.Sources {
		byID[reported.ID] = reported
	}
	for index := range state.Sources {
		reported, ok := byID[state.Sources[index].ID]
		if !ok {
			continue
		}
		state.Sources[index].Providers = append([]string{}, reported.Providers...)
		state.Sources[index].Error = reported.Error
		if state.Sources[index].Name == "" && reported.Name != "" {
			state.Sources[index].Name = reported.Name
		}
	}
	return state, nil
}

// ImportLxSource opens the OS file picker filtered to .js files, imports the
// chosen script and returns its row. A cancelled picker returns nil, nil.
func (s *LxSourcesService) ImportLxSource(ctx context.Context) (*LxSourceDTO, error) {
	path, err := openLxSourcePicker()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	source, err := s.svc.Import(ctx, path)
	if err != nil {
		return nil, translateLxError(err)
	}
	return &LxSourceDTO{
		ID:        source.ID,
		Name:      source.Name,
		FileName:  baseName(source.FilePath),
		Enabled:   source.Enabled,
		Position:  source.Position,
		Providers: []string{},
	}, nil
}

// SetLxSourceEnabled toggles one source. The plugin restarts so the change takes
// effect immediately.
func (s *LxSourcesService) SetLxSourceEnabled(ctx context.Context, id string, enabled bool) error {
	if err := s.svc.SetEnabled(ctx, id, enabled); err != nil {
		return translateLxError(err)
	}
	return nil
}

// RenameLxSource changes the display label of one source.
func (s *LxSourcesService) RenameLxSource(ctx context.Context, id, name string) error {
	if err := s.svc.Rename(ctx, id, name); err != nil {
		return translateLxError(err)
	}
	return nil
}

// RemoveLxSource deletes one source. The UI asks for confirmation first; the
// binding does not, because a confirmation is a presentation concern.
func (s *LxSourcesService) RemoveLxSource(ctx context.Context, id string) error {
	if err := s.svc.Remove(ctx, id); err != nil {
		return translateLxError(err)
	}
	return nil
}

// ReorderLxSources applies a new try order. ids must contain every source, in
// the new order; the list is then renumbered 0..n-1.
func (s *LxSourcesService) ReorderLxSources(ctx context.Context, ids []string) error {
	if err := s.svc.Reorder(ctx, ids); err != nil {
		return translateLxError(err)
	}
	return nil
}

// translateLxError turns lx sentinels into text a user can act on. The wrapped
// error is kept so callers can still classify it.
func translateLxError(err error) error {
	switch {
	case errors.Is(err, domain.ErrLxSourceInvalid):
		return fmt.Errorf("this file cannot be used as an lx source: %w", err)
	case errors.Is(err, domain.ErrLxSourceNotFound):
		return fmt.Errorf("this source is no longer in the list: %w", err)
	case errors.Is(err, domain.ErrLxSourceLimit):
		return fmt.Errorf("too many sources are imported; remove one first: %w", err)
	case errors.Is(err, domain.ErrLxPluginUnavailable):
		return fmt.Errorf("the Lx plugin is not available: %w", err)
	}
	return err
}
