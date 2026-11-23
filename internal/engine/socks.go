package engine

// Minimal SOCKS5 CONNECT client (RFC 1928, user/password RFC 1929), so a path can go
// through any SOCKS5 proxy (path types tcp and socks5).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

type socksProxy struct {
	addr       string
	user, pass string
}

func parseSocks(spec string) (*socksProxy, error) {
	if spec == "" {
		return nil, nil
	}
	u, err := url.Parse(spec)
	if err != nil || u.Scheme != "socks5" || u.Host == "" {
		return nil, fmt.Errorf("want socks5://[user:pass@]host:port, got %q", spec)
	}
	p := &socksProxy{addr: u.Host}
	if u.User != nil {
		p.user = u.User.Username()
		p.pass, _ = u.User.Password()
	}
	return p, nil
}

// dial connects to target through the proxy with d (its Control applies to the proxy socket).
func (p *socksProxy) dial(d *net.Dialer, target string) (net.Conn, error) {
	return p.dialContext(context.Background(), d, target)
}

// dialContext is dial that gives up when ctx is done.
func (p *socksProxy) dialContext(ctx context.Context, d *net.Dialer, target string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	c, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	// a context that ends during the exchange with the proxy ends it by closing c
	stop := context.AfterFunc(ctx, func() { c.Close() })
	c.SetDeadline(time.Now().Add(15 * time.Second))
	err = p.handshake(c, host, port)
	if !stop() {
		c.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("socks5 %s: %w", p.addr, err)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func (p *socksProxy) handshake(c net.Conn, host string, port int) error {
	method := byte(0)
	if p.user != "" {
		method = 2
	}
	if _, err := c.Write([]byte{5, 1, method}); err != nil {
		return err
	}
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return err
	}
	if b[0] != 5 || b[1] != method {
		return errors.New("proxy refused the auth method")
	}
	if method == 2 {
		if len(p.user) > 255 || len(p.pass) > 255 {
			return errors.New("user or password too long")
		}
		m := []byte{1, byte(len(p.user))}
		m = append(m, p.user...)
		m = append(m, byte(len(p.pass)))
		m = append(m, p.pass...)
		if _, err := c.Write(m); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		if b[1] != 0 {
			return errors.New("authentication failed")
		}
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = append(append(req, 1), ip.To4()...)
	} else if ip != nil {
		req = append(append(req, 4), ip.To16()...)
	} else {
		if len(host) > 255 {
			return errors.New("host name too long")
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	if h[1] != 0 {
		return fmt.Errorf("connect failed (reply %d)", h[1])
	}
	skip := 0
	switch h[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return err
		}
		skip = int(l[0])
	default:
		return errors.New("bad reply address type")
	}
	_, err := io.ReadFull(c, make([]byte, skip+2))
	return err
}
