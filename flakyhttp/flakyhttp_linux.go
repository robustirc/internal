//go:build linux

package flakyhttp

import "github.com/rjeczalik/notify"

var notifyEvents = []notify.Event{
	notify.InCloseWrite, // ioutil.WriteFile (write in-place)
	notify.InMovedTo,    // renameio.WriteFile (write with temporary file)
}
