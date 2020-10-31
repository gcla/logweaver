// +build !windows

package main

import (
	"os"

	"github.com/efekarakus/termcolor"
)

func maxColors() int {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return 0
	}
	defer tty.Close()
	switch l := termcolor.SupportLevel(tty); l {
	case termcolor.Level16M:
		return 256 // don't want more
	case termcolor.Level256:
		return 256
	case termcolor.LevelBasic:
		return 8
	default:
		return 0
	}
}
