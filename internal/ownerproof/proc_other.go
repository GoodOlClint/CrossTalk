//go:build !darwin && !linux

package ownerproof

func info(int) (Proc, error)             { return Proc{}, ErrUnsupported }
func args(int) (string, []string, error) { return "", nil, ErrUnsupported }
func lockHeld(string) (bool, error)      { return false, ErrUnsupported }
func openers(string) ([]int, error)      { return nil, ErrUnsupported }
