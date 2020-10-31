// +build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	shellquote "github.com/kballard/go-shellquote"
)

func maybeExecWithPager(color bool) bool {
	fileInfo, err := os.Stdout.Stat()
	// If we're running with stdout as a terminal, then helpfully redirect to a pager
	if err == nil && (fileInfo.Mode()&os.ModeCharDevice) != 0 {
		lm := shellquote.Join(os.Args...)
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGCHLD)

		pager := os.Getenv("PAGER")
		if pager == "" {
			if color {
				pager = "less -r"
			} else {
				pager = "less"
			}
		}

		os.StartProcess("/bin/sh", []string{"/bin/sh", "-c", lm + fmt.Sprintf(" | %s", pager)}, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			Env:   append(os.Environ(), "LOGWEAVER_USE_COLOR=true"),
		})

		quitChan := make(chan struct{}, 0)

		go func() {
			<-sigc
			quitChan <- struct{}{}
		}()

		<-quitChan
		return true
	}
	return false
}
