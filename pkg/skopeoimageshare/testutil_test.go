package skopeoimageshare

import "os"

// readFileImpl is a tiny shim so test files don't have to import os
// directly for one call.
func readFileImpl(p string) ([]byte, error) { return os.ReadFile(p) }
