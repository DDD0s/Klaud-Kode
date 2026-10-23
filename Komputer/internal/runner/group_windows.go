package runner

import "os/exec"

// taskkill /t walks the child tree, so no console group is needed here.
func setProcessGroup(cmd *exec.Cmd) {}
