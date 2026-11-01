package manifest

import "fmt"

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
