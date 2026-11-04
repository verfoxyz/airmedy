package wails

import (
	"fmt"
	"path/filepath"

	"airmedy/internal/domain"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// openLxSourcePicker opens the OS file picker filtered to lx custom-source
// scripts. It returns the chosen path, or "" when the user cancelled.
//
// It lives in this file rather than in the translation-only binding because a
// dialog is a side effect the binding must not own: the binding decides *what*
// to import, the shell decides *how* to ask.
func openLxSourcePicker() (string, error) {
	app := application.Get()
	if app == nil {
		return "", fmt.Errorf("%w: the application is not initialized", domain.ErrLxPluginUnavailable)
	}
	path, err := app.Dialog.OpenFile().
		SetTitle("Import lx custom source").
		AddFilter("lx custom source", "*"+domain.LxSourceExtension).
		PromptForSingleSelection()
	if err != nil {
		return "", fmt.Errorf("open lx source picker: %w", err)
	}
	return path, nil
}

// baseName is filepath.Base for the DTO. Kept here so the binding file has one
// small helper instead of importing path handling into every call site.
func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}
