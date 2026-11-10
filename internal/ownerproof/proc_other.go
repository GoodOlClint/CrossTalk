//go:build !darwin && !linux

package ownerproof

func info(int) (Proc, error)                    { return Proc{}, ErrUnsupported }
func args(int) (string, []string, error)        { return "", nil, ErrUnsupported }
func lockHeld(string) (bool, error)             { return false, ErrUnsupported }
func userPIDs() ([]int, error)                  { return nil, ErrUnsupported }
func openersAmong(string, []int) ([]int, error) { return nil, ErrUnsupported }
func executableIs(int, string) (bool, error)    { return false, ErrUnsupported }
