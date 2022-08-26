//go:build linux
// +build linux

package gnet


func(el *eventloop)startPolling(callback func(fd int) error)error{
	return el.poller.Polling()
}