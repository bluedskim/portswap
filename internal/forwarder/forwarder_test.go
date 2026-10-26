package forwarder

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// echoServer 는 받은 줄 앞에 이름을 붙여 돌려주는 테스트용 백엔드다.
func echoServer(t *testing.T, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					fmt.Fprintf(c, "%s:%s", name, line)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func startForwarder(t *testing.T) *Forwarder {
	t.Helper()
	f, err := New("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Serve(ctx)
	return f
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", msg); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestForwardsToCurrentBackend(t *testing.T) {
	a := echoServer(t, "A")
	f := startForwarder(t)
	f.SetBackend(a)

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "hi"); got != "A:hi\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSwitchKeepsExistingConnectionOnOldBackend(t *testing.T) {
	a := echoServer(t, "A")
	b := echoServer(t, "B")
	f := startForwarder(t)
	f.SetBackend(a)

	old, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if got := roundTrip(t, old, "1"); got != "A:1\n" {
		t.Fatalf("got %q", got)
	}

	f.SetBackend(b)

	// 기존 연결은 여전히 A 로 간다.
	if got := roundTrip(t, old, "2"); got != "A:2\n" {
		t.Fatalf("existing conn moved: %q", got)
	}
	// 새 연결은 B 로 간다.
	nw, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nw.Close()
	if got := roundTrip(t, nw, "3"); got != "B:3\n" {
		t.Fatalf("new conn not switched: %q", got)
	}
	if f.ActiveConns(a) != 1 || f.ActiveConns(b) != 1 {
		t.Fatalf("active counts a=%d b=%d", f.ActiveConns(a), f.ActiveConns(b))
	}
}

func TestWaitDrained(t *testing.T) {
	a := echoServer(t, "A")
	f := startForwarder(t)
	f.SetBackend(a)

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, "x") // 연결이 실제로 추적될 때까지 보장

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if f.WaitDrained(ctx, a) {
		t.Fatal("should not be drained while conn open")
	}
	c.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if !f.WaitDrained(ctx2, a) {
		t.Fatal("should drain after close")
	}
}

func TestNoBackendClosesConnection(t *testing.T) {
	f := startForwarder(t)
	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected EOF")
	}
}
