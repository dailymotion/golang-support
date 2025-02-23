package uws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/pyke369/golang-support/bslab"
	"github.com/pyke369/golang-support/rcache"
	"github.com/pyke369/golang-support/uuid"
	"golang.org/x/net/http/httpproxy"
)

const (
	WEBSOCKET_UUID            = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	WEBSOCKET_VERSION         = "13"
	WEBSOCKET_FIN             = 0x80
	WEBSOCKET_MASK            = 0x80
	WEBSOCKET_OPCODE_TEXT     = 1
	WEBSOCKET_OPCODE_BLOB     = 2
	WEBSOCKET_OPCODE_CLOSE    = 8
	WEBSOCKET_OPCODE_PING     = 9
	WEBSOCKET_OPCODE_PONG     = 10
	WEBSOCKET_ERROR_PROTOCOL  = 1002
	WEBSOCKET_ERROR_INVALID   = 1007
	WEBSOCKET_ERROR_OVERSIZED = 1009
)

type Config struct {
	Proxy           func(*url.URL) (*url.URL, error)
	TLSConfig       *tls.Config
	Headers         map[string]string
	Protocols       []string
	NeedProtocol    bool
	ReadSize        int
	FragmentSize    int
	MessageSize     int
	ConnectTimeout  time.Duration
	ProbeTimeout    int64
	InactiveTimeout int64
	WriteTimeout    int64
	WriteBufferSize int
	ReadBufferSize  int
	OpenHandler     func(*Socket)
	MessageHandler  func(*Socket, int, []byte) bool
	CloseHandler    func(*Socket, int)
	Context         any
}

type Socket struct {
	Path, Origin, Agent, Remote, Protocol string
	Context                               any
	config                                *Config
	conn                                  net.Conn
	connected, client, closing            bool
	wlock, dlock, clock                   sync.Mutex
	slast, rlast                          int64
}

var (
	proxy func(*url.URL) (*url.URL, error)
	now   int64
)

func init() {
	proxy = httpproxy.FromEnvironment().ProxyFunc()
	atomic.StoreInt64(&now, time.Now().UnixNano())
	go func() {
		for {
			atomic.StoreInt64(&now, time.Now().UnixNano())
			time.Sleep(250 * time.Millisecond)
		}
	}()
}

func Dial(endpoint, origin string, config *Config) (ws *Socket, err error) {
	if config == nil {
		config = &Config{}
	}
	if config.Proxy == nil {
		config.Proxy = proxy
	}
	config.ReadSize = cval(config.ReadSize, 4<<10, 4<<10, 256<<10)
	config.FragmentSize = cval(config.FragmentSize, 16<<10, 4<<10, 1<<20)
	config.MessageSize = cval(config.MessageSize, 4<<20, 4<<10, 64<<20)
	config.ConnectTimeout = time.Duration(cval(int(config.ProbeTimeout), int(10*time.Second), int(1*time.Second), int(30*time.Second)))
	config.ProbeTimeout = int64(cval(int(config.ProbeTimeout), int(15*time.Second), int(1*time.Second), int(30*time.Second)))
	config.InactiveTimeout = int64(cval(int(config.InactiveTimeout), int(3*config.ProbeTimeout), int(config.ProbeTimeout+int64(time.Second)), int(5*config.ProbeTimeout)))
	config.WriteTimeout = int64(cval(int(config.WriteTimeout), int(10*time.Second), int(1*time.Second), int(30*time.Second)))
	if config.ReadBufferSize != 0 {
		config.ReadBufferSize = cval(config.ReadBufferSize, 4<<10, 4<<10, 32<<20)
	}
	if config.WriteBufferSize != 0 {
		config.WriteBufferSize = cval(config.WriteBufferSize, 4<<10, 4<<10, 32<<20)
	}
	endpoint = strings.Replace(strings.Replace(endpoint, "ws:", "http:", 1), "wss:", "https:", 1)
	if url, err := url.Parse(endpoint); err == nil {
		proxy, _ := config.Proxy(url)
		if request, err := http.NewRequest("GET", endpoint, nil); err == nil {
			nonce := base64.StdEncoding.EncodeToString(uuid.BUUID())
			request.Header.Add("User-Agent", "uws")
			request.Header.Add("Connection", "Upgrade")
			request.Header.Add("Upgrade", "websocket")
			request.Header.Add("Sec-WebSocket-Version", WEBSOCKET_VERSION)
			request.Header.Add("Sec-WebSocket-Key", nonce)
			if len(config.Protocols) > 0 {
				request.Header.Add("Sec-WebSocket-Protocol", strings.Join(config.Protocols, ", "))
			}
			if origin != "" {
				request.Header.Add("Origin", origin)
			}
			for name, value := range config.Headers {
				request.Header.Add(name, value)
			}

			start, scheme, address := time.Now(), url.Scheme, url.Host
			if proxy != nil {
				scheme, address = proxy.Scheme, proxy.Host
			}
			ctx, cancel := context.WithTimeout(context.Background(), config.ConnectTimeout)
			defer cancel()
			if conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address); err == nil {
				if tconn, ok := conn.(*net.TCPConn); ok {
					if config.ReadBufferSize != 0 {
						tconn.SetReadBuffer(config.ReadBufferSize)
					}
					if config.WriteBufferSize != 0 {
						tconn.SetWriteBuffer(config.WriteBufferSize)
					}
				}
				if scheme == "https" {
					if config.TLSConfig == nil {
						config.TLSConfig = &tls.Config{}
					}
					config.TLSConfig.ServerName = address
					if value, _, err := net.SplitHostPort(address); err == nil {
						config.TLSConfig.ServerName = value
					}
					conn = tls.Client(conn, config.TLSConfig)
					if err := conn.(*tls.Conn).HandshakeContext(ctx); err != nil {
						conn.Close()
						return nil, fmt.Errorf(`websocket: %v`, err)
					}
				}
				if proxy != nil {
					host, port := url.Host, "0"
					if value1, value2, err := net.SplitHostPort(host); err == nil {
						host, port = value1, value2
					}
					if port == "0" {
						if url.Scheme == "https" {
							port = "443"
						} else {
							port = "80"
						}
					}
					payload := fmt.Sprintf("CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n", host, port, host, port)
					if user := proxy.User; user != nil {
						password, _ := user.Password()
						payload += fmt.Sprintf("Proxy-Authorization: basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
					}
					payload += "\r\n"

					conn.SetWriteDeadline(time.Now().Add(config.ConnectTimeout - time.Since(start)))
					if _, err := conn.Write([]byte(payload)); err != nil {
						conn.Close()
						return nil, fmt.Errorf(`websocket: %v`, err)
					}
					conn.SetReadDeadline(time.Now().Add(config.ConnectTimeout))
					if response, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
						response.Body.Close()
						if response.StatusCode != 200 {
							conn.Close()
							return nil, fmt.Errorf(`websocket: proxy connection error`)
						}
					} else {
						conn.Close()
						return nil, fmt.Errorf(`websocket: %v`, err)
					}

					if url.Scheme == "https" {
						if config.TLSConfig == nil {
							config.TLSConfig = &tls.Config{}
						}
						config.TLSConfig.ServerName = host
						conn = tls.Client(conn, config.TLSConfig)
						if err := conn.(*tls.Conn).HandshakeContext(ctx); err != nil {
							conn.Close()
							return nil, fmt.Errorf(`websocket: %v`, err)
						}
					}
				}

				conn.SetWriteDeadline(time.Now().Add(config.ConnectTimeout - time.Since(start)))
				if err := request.Write(conn); err != nil {
					conn.Close()
					return nil, fmt.Errorf(`websocket: %v`, err)
				}
				conn.SetReadDeadline(time.Now().Add(config.ConnectTimeout))
				if response, err := http.ReadResponse(bufio.NewReader(conn), request); err == nil {
					skey, _ := base64.StdEncoding.DecodeString(response.Header.Get("Sec-WebSocket-Accept"))
					ckey, path := sha1.Sum([]byte(nonce+WEBSOCKET_UUID)), url.Path
					if path == "" {
						path = "/"
					}
					if response.StatusCode != http.StatusSwitchingProtocols || strings.ToLower(response.Header.Get("Connection")) != "upgrade" ||
						strings.ToLower(response.Header.Get("Upgrade")) != "websocket" || !bytes.Equal(ckey[:], skey) {
						response.Body.Close()
						conn.Close()
						return nil, fmt.Errorf(`websocket: invalid protocol upgrade (status %d)`, response.StatusCode)
					}
					protocol := response.Header.Get("Sec-WebSocket-Protocol")
					if len(config.Protocols) > 0 && protocol == "" && config.NeedProtocol {
						response.Body.Close()
						conn.Close()
						return nil, errors.New(`websocket: could not negotiate sub-protocol with server`)
					}
					ws = &Socket{Path: path, Remote: conn.RemoteAddr().String(), Origin: origin, Protocol: protocol, Context: config.Context,
						config: config, client: true, conn: conn, connected: true}
					go ws.receive(nil)
					if config.OpenHandler != nil {
						config.OpenHandler(ws)
					}
				} else {
					conn.Close()
					return nil, err
				}
			} else {
				return nil, fmt.Errorf(`websocket: %v`, err)
			}
		} else {
			return nil, fmt.Errorf(`websocket: %v`, err)
		}
	} else {
		return nil, fmt.Errorf(`websocket: %v`, err)
	}
	return
}

func Handle(response http.ResponseWriter, request *http.Request, config *Config) (handled bool, ws *Socket) {
	if strings.Contains(strings.ToLower(request.Header.Get("Connection")), "upgrade") && strings.ToLower(request.Header.Get("Upgrade")) == "websocket" {
		handled = true
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ckey := request.Header.Get("Sec-WebSocket-Key")
		if request.Header.Get("Sec-WebSocket-Version") != WEBSOCKET_VERSION || ckey == "" {
			response.Header().Set("Sec-WebSocket-Version", WEBSOCKET_VERSION)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := response.(http.Hijacker); !ok {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		cprotocols, sprotocols, protocol := []string{}, map[string]bool{}, ""
		if len(config.Protocols) > 0 {
			if splitter := rcache.Get("[, ]+"); splitter != nil {
				cprotocols = splitter.Split(request.Header.Get("Sec-WebSocket-Protocol"), 10)
			}
			if len(cprotocols) > 0 {
				for _, value := range config.Protocols {
					sprotocols[value] = true
				}
				for _, value := range cprotocols {
					if sprotocols[value] {
						protocol = value
					}
				}
			}
			if protocol != "" {
				response.Header().Set("Sec-WebSocket-Protocol", protocol)
			} else if config.NeedProtocol {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		skey := sha1.Sum([]byte(ckey + WEBSOCKET_UUID))
		response.Header().Set("Connection", "Upgrade")
		response.Header().Set("Upgrade", "websocket")
		response.Header().Set("Sec-WebSocket-Accept", base64.StdEncoding.EncodeToString(skey[:]))
		response.WriteHeader(http.StatusSwitchingProtocols)
		if conn, reader, err := response.(http.Hijacker).Hijack(); err == nil {
			conn.SetDeadline(time.Time{})
			if config == nil {
				config = &Config{}
			}
			config.ReadSize = cval(config.ReadSize, 4<<10, 4<<10, 256<<10)
			config.FragmentSize = cval(config.FragmentSize, 16<<10, 4<<10, 1<<20)
			config.MessageSize = cval(config.MessageSize, 4<<20, 4<<10, 64<<20)
			config.ProbeTimeout = int64(cval(int(config.ProbeTimeout), int(10*time.Second), int(1*time.Second), int(30*time.Second)))
			config.InactiveTimeout = int64(cval(int(config.InactiveTimeout), int(3*config.ProbeTimeout), int(config.ProbeTimeout+int64(time.Second)), int(5*config.ProbeTimeout)))
			config.WriteTimeout = int64(cval(int(config.WriteTimeout), int(10*time.Second), int(1*time.Second), int(30*time.Second)))
			if config.ReadBufferSize != 0 {
				config.ReadBufferSize = cval(config.ReadBufferSize, 4<<10, 4<<10, 32<<20)
			}
			if config.WriteBufferSize != 0 {
				config.WriteBufferSize = cval(config.WriteBufferSize, 4<<10, 4<<10, 32<<20)
			}
			if tconn, ok := conn.(*net.TCPConn); ok {
				if config.ReadBufferSize != 0 {
					tconn.SetReadBuffer(config.ReadBufferSize)
				}
				if config.WriteBufferSize != 0 {
					tconn.SetWriteBuffer(config.WriteBufferSize)
				}
			}
			origin := request.Header.Get("Origin")
			if strings.ToLower(origin) == "null" {
				origin = ""
			}
			ws = &Socket{Path: request.URL.Path, Origin: origin, Agent: request.Header.Get("User-Agent"),
				Remote: conn.RemoteAddr().String(), Protocol: protocol, Context: config.Context, config: config, conn: conn, connected: true}
			go ws.receive(reader)
			if config.OpenHandler != nil {
				config.OpenHandler(ws)
			}
		}
		return
	}
	return
}

func (s *Socket) IsClient() bool {
	return s.client
}

func (s *Socket) IsConnected() bool {
	return s.connected
}

func (s *Socket) Write(mode byte, data []byte) (err error) {
	var mask []byte

	length := len(data)
	if (mode == WEBSOCKET_OPCODE_TEXT || mode == WEBSOCKET_OPCODE_BLOB) && length > 0 {
		s.dlock.Lock()
		defer s.dlock.Unlock()
		frames := length / s.config.FragmentSize
		if length%s.config.FragmentSize != 0 {
			frames++
		}
		for frame := 1; frame <= frames; frame++ {
			fin, offset, size := byte(0), (frame-1)*s.config.FragmentSize, s.config.FragmentSize
			if frame == frames {
				fin, size = WEBSOCKET_FIN, length-offset
			}
			if frame > 1 {
				mode = 0
			}
			payload := net.Buffers{[]byte{fin | mode, 0}}
			if size < 126 {
				payload[0][1] |= byte(size)
			} else if size < 65536 {
				payload[0][1] |= 126
				payload = append(payload, []byte{0, 0})
				binary.BigEndian.PutUint16(payload[1], uint16(size))
			} else {
				payload[0][1] |= 127
				payload = append(payload, []byte{0, 0, 0, 0, 0, 0, 0, 0})
				binary.BigEndian.PutUint64(payload[1], uint64(size))
			}
			if s.client {
				payload[0][1] |= WEBSOCKET_MASK
				mask = rmask()
				payload = append(payload, mask)
				xor(mask, data[offset:offset+size])
			}
			payload = append(payload, data[offset:offset+size])
			err = s.send(payload)
			if s.client {
				xor(mask, data[offset:offset+size])
			}
			if err != nil {
				return
			}
		}
	}
	return
}

func (s *Socket) Close(code int) {
	s.clock.Lock()
	if !s.closing && s.connected {
		s.closing = true
		s.clock.Unlock()
		if s.config != nil && s.config.CloseHandler != nil {
			s.config.CloseHandler(s, code)
		}
		payload := net.Buffers{[]byte{WEBSOCKET_FIN | WEBSOCKET_OPCODE_CLOSE, 0}}
		if s.client {
			payload[0][1] |= WEBSOCKET_MASK
			payload = append(payload, rmask())
		}
		if code != 0 {
			payload[0][1] |= 2
			payload = append(payload, []byte{0, 0})
			binary.BigEndian.PutUint16(payload[len(payload)-1], uint16(code))
			if s.client {
				xor(payload[1], payload[2])
			}
		}
		s.send(payload)
		s.connected = false
		s.conn.Close()
	} else {
		s.clock.Unlock()
	}
}

func (s *Socket) send(payload net.Buffers) (err error) {
	if !s.connected {
		return errors.New(`websocket: not connected`)
	}
	s.wlock.Lock()
	lnow := atomic.LoadInt64(&now)
	if time.Duration(lnow-s.slast) >= time.Second {
		s.slast = lnow
		s.conn.SetWriteDeadline(time.UnixMicro(lnow / int64(time.Microsecond)).Add(time.Duration(s.config.WriteTimeout)))
	}
	if _, err = payload.WriteTo(s.conn); err != nil {
		s.wlock.Unlock()
		s.Close(0)
	} else {
		s.wlock.Unlock()
	}
	return
}

func (s *Socket) receive(buffered io.Reader) {
	var data, control []byte
	var err error

	fin, opcode, size, mask, smask := byte(0), byte(0), -1, make([]byte, 4), 0
	seen, code, dmode, dsize, doffset, dlast := atomic.LoadInt64(&now), 0, byte(0), 0, 0, false
	buffer, roffset, woffset, read := bslab.Get(s.config.ReadSize, nil), 0, 0, 0
	buffer = buffer[:cap(buffer)]
	if !s.client {
		smask += 4
	}
close:
	for {
		if cap(buffer)-roffset < 14 {
			copy(buffer[0:], buffer[roffset:woffset])
			woffset -= roffset
			roffset = 0
		}

		lnow := atomic.LoadInt64(&now)
		if time.Duration(lnow-s.rlast) >= time.Second {
			s.rlast = lnow
			s.conn.SetReadDeadline(time.UnixMicro(lnow / int64(time.Microsecond)).Add(time.Duration(s.config.ProbeTimeout)))
		}
		if buffered != nil {
			read, err = buffered.Read(buffer[woffset:])
			buffered = nil
		} else {
			read, err = s.conn.Read(buffer[woffset:])
		}

		if read > 0 {
			seen = atomic.LoadInt64(&now)
			woffset += read
			for {
				if size < 0 {
					if woffset-roffset >= 2 {
						fin, opcode, size = buffer[roffset]>>7, buffer[roffset]&0x0f, int(buffer[roffset+1]&0x7f)
						if (s.client && (buffer[roffset+1]&WEBSOCKET_MASK) != 0) || (!s.client && (buffer[roffset+1]&WEBSOCKET_MASK) == 0) ||
							(fin == 0 && opcode >= WEBSOCKET_OPCODE_CLOSE && opcode <= WEBSOCKET_OPCODE_PONG) ||
							(opcode != 0 && opcode != WEBSOCKET_OPCODE_TEXT && opcode != WEBSOCKET_OPCODE_BLOB && (opcode < WEBSOCKET_OPCODE_CLOSE || opcode > WEBSOCKET_OPCODE_PONG)) {
							code = WEBSOCKET_ERROR_PROTOCOL
							break close
						}
						if !s.client && woffset-roffset < 2+smask {
							size = -1
							break
						}
						if opcode == WEBSOCKET_OPCODE_TEXT || opcode == WEBSOCKET_OPCODE_BLOB {
							dmode = opcode
						}
						if dmode != 0 && fin == 1 {
							dlast = true
						}
						if size == 126 {
							if woffset-roffset < 4+smask {
								size = -1
								break
							}
							size = int(binary.BigEndian.Uint16(buffer[roffset+2:]))
							if !s.client {
								copy(mask, buffer[roffset+4:])
							}
							roffset += 4 + smask
						} else if size == 127 {
							if woffset-roffset < 10+smask {
								size = -1
								break
							}
							size = int(binary.BigEndian.Uint64(buffer[roffset+2:]))
							if !s.client {
								copy(mask, buffer[roffset+10:])
							}
							roffset += 10 + smask
						} else {
							if !s.client {
								copy(mask, buffer[roffset+2:])
							}
							roffset += 2 + smask
						}
						if (opcode <= WEBSOCKET_OPCODE_BLOB && size == 0) || (opcode > WEBSOCKET_OPCODE_BLOB && size > 125) || (fin == 1 && size > s.config.MessageSize) {
							code = WEBSOCKET_ERROR_OVERSIZED
							break close
						}
						if dmode != 0 {
							dsize += size
						}
					} else {
						break
					}
				}

				if size >= 0 {
					if dmode != 0 {
						if data == nil {
							data = bslab.Get(dsize, nil)
						}
						max := int(math.Min(float64(woffset-roffset), float64(size)))
						if len(data)+max > s.config.MessageSize {
							code = WEBSOCKET_ERROR_OVERSIZED
							break close
						}
						data = append(data, buffer[roffset:roffset+max]...)
						size -= max
						roffset += max
						if size <= 0 && len(data) >= dsize {
							if !s.client {
								xor(mask, data[doffset:dsize])
							}
							doffset = dsize
							if dlast {
								if dmode == WEBSOCKET_OPCODE_TEXT && !utf8.Valid(data) {
									code = WEBSOCKET_ERROR_INVALID
									break close
								}
								keep := false
								if s.config.MessageHandler != nil {
									keep = s.config.MessageHandler(s, int(dmode), data)
								}
								if !keep {
									bslab.Put(data)
								}
								dmode, dsize, doffset, dlast, data = 0, 0, 0, false, nil
							}
							size = -1
						}
					} else {
						if control == nil {
							control = bslab.Get(132, nil)
						}
						max := int(math.Min(float64(woffset-roffset), float64(size)))
						control = append(control, buffer[roffset:roffset+max]...)
						size -= max
						roffset += max
						if size <= 0 {
							if !s.client {
								xor(mask, control)
							}
							switch opcode {
							case WEBSOCKET_OPCODE_CLOSE:
								if len(control) >= 2 {
									code = int(binary.BigEndian.Uint16(control))
								}
								break close
							case WEBSOCKET_OPCODE_PING:
								payload := net.Buffers{[]byte{WEBSOCKET_FIN | WEBSOCKET_OPCODE_PONG, byte(len(control))}}
								if s.client {
									payload[0][1] |= WEBSOCKET_MASK
									payload = append(payload, rmask())
									xor(payload[1], control)
								}
								payload = append(payload, control)
								if err := s.send(payload); err != nil {
									break close
								}
							}
							bslab.Put(control)
							size, control = -1, nil
						}
					}
				}

				if roffset >= woffset {
					roffset, woffset = 0, 0
					break
				}
			}
		}

		if err != nil {
			if err, ok := err.(net.Error); ok && err.Timeout() {
				payload := net.Buffers{[]byte{WEBSOCKET_FIN | WEBSOCKET_OPCODE_PING, 0}}
				if s.client {
					payload[0][1] |= WEBSOCKET_MASK
					payload = append(payload, rmask())
				}
				if err := s.send(payload); err != nil {
					break close
				}
			} else {
				break close
			}
		} else if read == 0 {
			break close
		}

		if atomic.LoadInt64(&now)-seen >= s.config.InactiveTimeout {
			break close
		}
	}
	bslab.Put(buffer)
	bslab.Put(control)
	bslab.Put(data)
	s.Close(code)
}

func rmask() []byte {
	value := []byte{0, 0, 0, 0}
	rand.Read(value)
	return value
}

func cval(value, fallback, min, max int) int {
	if value == 0 {
		value = fallback
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

const xorsize = int(unsafe.Sizeof(uintptr(0)))

func xor(mask []byte, data []byte) {
	offset, length := 0, len(data)
	if length >= xorsize {
		var value [xorsize]byte

		for index := range value {
			value[index] = mask[index%4]
		}
		xorer := *(*uintptr)(unsafe.Pointer(&value))
		offset = (length / xorsize) * xorsize
		for index := 0; index < offset; index += xorsize {
			*(*uintptr)(unsafe.Pointer(uintptr(unsafe.Pointer(&data[0])) + uintptr(index))) ^= xorer
		}
	}
	for index := offset; index < length; index++ {
		data[index] ^= mask[(index-offset)%4]
	}
}
