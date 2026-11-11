package domain

import "errors"

// ErrLastFmNotConnected means the feature that needs the user's Last.fm
// account was asked while no session or username is stored.
//
// Read-only Last.fm calls need a username even though they do not need the
// session key, so both are checked at the same boundary: a page that cannot
// name the user cannot personalise anything, and the UI shows the connect
// guidance instead of an empty recommendation list.
var ErrLastFmNotConnected = errors.New("last.fm is not connected")
