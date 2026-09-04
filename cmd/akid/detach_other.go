//go:build !linux

package main

import "os/exec"

func configureDetached(*exec.Cmd) {}
