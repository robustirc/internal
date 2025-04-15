//go:build !linux

package flakyhttp

import "github.com/rjeczalik/notify"

// TODO: test with other operating systems and put in their respective constants
var notifyEvents = []notify.Event{}
