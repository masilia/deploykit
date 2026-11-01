package transport

// rsyncArgs builds the rsync argument list. remoteShell is the value of -e ("" for a local copy). The destination of
// a remote sync is prefixed by the caller; this function only assembles the flags and the two paths.
func rsyncArgs(spec SyncSpec, remoteShell string) []string {
	args := []string{"-az"}
	if remoteShell != "" {
		args = append(args, "-e", remoteShell)
	}
	if spec.Delete {
		args = append(args, "--delete")
	}
	if spec.Checksum {
		args = append(args, "-c")
	}
	if spec.Stats {
		args = append(args, "--info=stats1")
	}
	if spec.LinkDest != "" {
		args = append(args, "--link-dest="+spec.LinkDest)
	}
	for _, e := range spec.Excludes {
		args = append(args, "--exclude="+e)
	}
	return append(args, spec.Src, spec.Dst)
}
