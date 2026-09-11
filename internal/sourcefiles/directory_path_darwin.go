//go:build darwin

package sourcefiles

func stableDescriptorRoot(_, _ int, label string) string { return label }
