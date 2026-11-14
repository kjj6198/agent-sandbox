package proto

import (
	"io"
	"net"
	"sync"
)

// Splice copies between a and b in both directions until both are done, forwarding
// half-closes, then closes both. ar and br read from a and b and may hold bytes that were
// already buffered while parsing an HTTP upgrade.
func Splice(a net.Conn, ar io.Reader, b net.Conn, br io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, ar)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, br)
		closeWrite(a)
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}
