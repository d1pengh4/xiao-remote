//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
)

var errRunning = errors.New("receiver already running")

func samePath(a, b string) bool                { return filepath.Clean(a) == filepath.Clean(b) }
func replaceFile(a, b string) error            { return os.Rename(a, b) }
func consoleAttached() bool                    { return false }
func acquireInstance(string) (func(), error)   { return func() {}, nil }
func instanceRunning(string) bool              { return false }
func startDetached(string, ...string) error    { return errors.New("Windows only") }
func findBrowser() (string, error)             { return "", errors.New("Windows only") }
func openBrowser(string, string, string) error { return errors.New("Windows only") }
func openStatus(string) error                  { return errors.New("Windows only") }
func notify(string, string)                    {}
func stopLegacy(paths, bool) error             { return nil }
func scheduleSelfRemoval()                     {}
