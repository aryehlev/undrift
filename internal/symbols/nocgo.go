//go:build !cgo

package symbols

// Available reports whether symbol analysis is compiled in.
func Available() bool { return false }

// Supported reports whether path has a grammar.
func Supported(string) bool { return false }

// Language returns the grammar name for path, or "".
func Language(string) string { return "" }

// Extract parses src and returns its definitions.
func Extract(string, []byte) ([]Symbol, error) { return nil, ErrUnsupported }
