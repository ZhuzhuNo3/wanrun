package runlogs

// Stream identifies the raw child descriptor represented by one log file.
type Stream uint8

const (
	PTY Stream = iota + 1
	Stdout
	Stderr
)
