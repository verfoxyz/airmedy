package domain

import (
	"context"
	"errors"
	"time"
)

// LxSource is one imported lx custom-source script.
//
// The lx plugin runs these scripts; the core owns the *list*: where the files
// live, whether each one is enabled, and in what order they are tried. The
// plugin is a child process with no persistence of its own, and the user manages
// the list from the UI, so the core has to know it (catalog/plugins/README.md,
// "Managed source lists").
//
// FilePath is an absolute path under the app data directory, not the file the
// user picked: the import copies the script so moving or deleting the original
// does not break playback.
type LxSource struct {
	ID       string `db:"id"`
	Name     string `db:"name"`
	FilePath string `db:"file_path"`
	Enabled  bool   `db:"enabled"`
	// Position is the try order: lower runs first. It is also the priority the
	// plugin walks when a source cannot resolve a track.
	Position  int       `db:"position"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// LxSourceRepository persists the imported lx source list.
type LxSourceRepository interface {
	// List returns every source ordered by position, then id so the order is
	// total even when two rows share a position.
	List(ctx context.Context) ([]LxSource, error)
	// Create inserts one source. A duplicate id is an error.
	Create(ctx context.Context, source LxSource) error
	// Update writes the editable fields (name, enabled) of an existing row.
	Update(ctx context.Context, source LxSource) error
	// Delete removes one row. Deleting a missing row is not an error: the file
	// may already be gone and the UI must still be able to clear the list.
	Delete(ctx context.Context, id string) error
	// Reorder assigns positions 0..n-1 in the given order. Unknown ids are
	// ignored, and the whole assignment is one transaction so a concurrent read
	// never sees two rows at the same priority.
	Reorder(ctx context.Context, ids []string) error
}

// PluginSourceDescription is one source as the running plugin reports it through
// `describe`. The core's LxSource row says what the user imported and enabled;
// this says what the plugin actually loaded, which is why the UI joins the two.
type PluginSourceDescription struct {
	// ID is the LxSource.ID the plugin was started with.
	ID string `json:"id"`
	// Name is the plugin's own label for it, if it has one.
	Name string `json:"name,omitempty"`
	// Enabled mirrors the flag the plugin was given.
	Enabled bool `json:"enabled"`
	// Providers are the platforms this script registered at load time.
	Providers []string `json:"providers,omitempty"`
	// Error explains a script that did not load (syntax error, no inited call).
	// Empty means it is healthy.
	Error string `json:"error,omitempty"`
}

// PluginDescribe is the answer to the host→plugin `describe` call.
//
// It exists because providers cannot be a static manifest field for a plugin
// whose content the user changes: the lx plugin's manifest declares no
// providers, and the routes are whatever the imported scripts support right
// now. The registry calls this after `initialize` and after every restart.
type PluginDescribe struct {
	// Providers is the union of the platforms every loaded source supports.
	// The app routes on this, so a source the user disabled is absent.
	Providers []string `json:"providers"`
	// Sources is the per-script detail, used by the settings page.
	Sources []PluginSourceDescription `json:"sources"`
	// SearchSources is the catalog sources this plugin can *search*.
	//
	// It is deliberately a separate list from Providers: streaming a track and
	// searching a catalogue are different endpoints with different failure
	// modes, and a plugin may do one without the other (the search plugin does
	// not resolve audio; the lx plugin resolves audio but cannot search). The
	// search screen's source buttons come from here, so "add a searchable
	// source" stays a plugin change and adds no protocol method
	// (docs/plugin-api.md §6.1: reuse describe, never plugin-to-plugin calls).
	SearchSources []CatalogSearchSource `json:"searchSources,omitempty"`
}

// Lx import limits. They live in the domain so the app and the binding enforce
// the same numbers, and so the error messages have one source of truth.
const (
	// MaxLxSourceBytes bounds an imported script. Real lx sources are tens of
	// kilobytes; anything past this is a mistake or an attack.
	MaxLxSourceBytes = 2 << 20 // 2 MiB
	// LxSourceExtension is the only file type the import accepts.
	LxSourceExtension = ".js"
)

// Lx source errors. The app returns these with a readable cause; callers
// classify with errors.Is.
var (
	// ErrLxSourceInvalid marks a file that is not a usable lx custom source.
	// The wrapped cause says why (wrong extension, too large, syntax error, or
	// no platform registered).
	ErrLxSourceInvalid = errors.New("lx source invalid")
	// ErrLxSourceNotFound marks a lookup or mutation for an unknown source id.
	ErrLxSourceNotFound = errors.New("lx source not found")
	// ErrLxSourceLimit marks an import rejected because the list is full. The
	// cap keeps a user from turning the plugin into a hundred sandboxes.
	ErrLxSourceLimit = errors.New("lx source limit reached")
	// ErrLxPluginUnavailable marks an import or refresh that cannot reach the
	// Lx plugin, because it is not installed or cannot start.
	ErrLxPluginUnavailable = errors.New("lx plugin unavailable")
)

// MethodValidateSource is the plugin method the import path calls to check a
// candidate script before the core stores it.
//
// The core deliberately does not parse or execute lx scripts: whether a file is
// a usable custom source is a question only the lx runtime can answer, and only
// the plugin owns that runtime. The host passes a absolute path, the plugin
// loads it in a throwaway sandbox and reports what it registered.
const MethodValidateSource = "source.validate"

// PluginValidateSourceRequest is the params object of source.validate.
type PluginValidateSourceRequest struct {
	// Path is an absolute path the plugin may read. Importing copies the file
	// first, so the plugin never touches the user's original after the import.
	Path string `json:"path"`
}

// PluginSourceValidation is the result object of source.validate.
//
// It is separate from PluginSourceDescription because validation describes a
// file that is not a list entry yet: it has no id and no enable flag.
type PluginSourceValidation struct {
	// Name is the display label the script declares for itself, if any.
	Name string `json:"name,omitempty"`
	// Providers are the platforms the script registers when it loads. An empty
	// list means "not a custom source" and is rejected by the import path.
	Providers []string `json:"providers"`
}
