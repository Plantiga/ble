package connection

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/rigado/ble/linux/hci"
	"io"
	"net"
	"time"

	"github.com/pkg/errors"
	"github.com/rigado/ble"
	"github.com/rigado/ble/linux/hci/cmd"
	"github.com/rigado/ble/linux/hci/evt"
)

// Conn ...
type Conn struct {
	ctrl hci.Controller
	ctx context.Context

	param evt.LEConnectionComplete

	// While MTU is the maximum size of payload data that the upper layer (ATT)
	// can accept, the MPS is the maximum PDU payload size this L2CAP implementation
	// supports. When segmantation is not used, the MPS should be made to the same
	// values of MTUs [Vol 3, Part A, 1.4].
	//
	// For LE-U logical transport, the L2CAP implementations should support
	// a minimum of 23 bytes, which are also the default values before the
	// upper layer (ATT) optionally reconfigures them [Vol 3, Part A, 3.2.8].
	rxMTU int
	txMTU int
	rxMPS int

	// Signaling MTUs are The maximum size of command information that the
	// L2CAP layer entity is capable of accepting.
	// A L2CAP implementations supporting LE-U should support at least 23 bytes.
	// Currently, we support 512 bytes, which should be more than sufficient.
	// The sigTxMTU is discovered via when we sent a signaling pkt that is
	// larger thean the remote device can handle, and get a response of "Command
	// Reject" indicating "Signaling MTU exceeded" along with the actual
	// signaling MTU [Vol 3, Part A, 4.1].
	sigRxMTU int
	sigTxMTU int

	sigSent chan []byte
	// smpSent chan []byte

	chInPkt chan Packet
	chInPDU chan Pdu

	chDone chan struct{}
	// Host to Controller Data Flow Control pkt-based Data flow control for LE-U [Vol 2, Part E, 4.1.1]
	// chSentBufs tracks the HCI buffer occupied by this connection.
	txBuffer hci.BufferPool

	// sigID is used to match responses with signaling requests.
	// The requesting device sets this field and the responding device uses the
	// same value in its response. Within each signalling channel a different
	// Identifier shall be used for each successive command. [Vol 3, Part A, 4]
	sigID uint8

	// leFrame is set to be true when the LE Credit based flow control is used.
	leFrame bool

	smp hci.SmpManager
}

type Encrypter interface {
	Encrypt() error
}

func New(ctrl hci.Controller, param evt.LEConnectionComplete) *Conn {

	c := &Conn{
		ctrl:  ctrl,
		ctx:   context.Background(),
		param: param,

		rxMTU: ble.DefaultMTU,
		txMTU: ble.DefaultMTU,

		rxMPS: ble.DefaultMTU,

		sigRxMTU: ble.MaxMTU,
		sigTxMTU: ble.DefaultMTU,

		chInPkt: make(chan Packet, 16),
		chInPDU: make(chan Pdu, 16),

		txBuffer: ctrl.RequestBufferPool(),

		chDone: make(chan struct{}),
	}

	smp, err := c.ctrl.RequestSmpManager(hci.DefaultSmpConfig)
	if err == nil {
		c.smp = smp
		c.initPairingContext()
		c.smp.SetWritePDUFunc(c.writePDU)
		c.smp.SetEncryptFunc(c.encrypt)
	}

	go func() {
		for {
			if err := c.recombine(); err != nil {
				if err != io.EOF {
					err = errors.Wrap(err, "recombine")
					c.ctrl.DispatchError(err)

					//attempt to cleanup
					//todo: this is the job of hci
					//if err := c.hci.cleanupConnectionHandle(c.param.ConnectionHandle()); err != nil {
					//	fmt.Printf("recombine cleanup: %v\n", err)
					//}
				} else {
					fmt.Println("recombine non io.EOF error:", err)
				}
				close(c.chInPDU)
				return
			}
		}
	}()
	return c
}

// Context returns the context that is used by this Conn.
func (c *Conn) Context() context.Context {
	return c.ctx
}

// SetContext sets the context that is used by this Conn.
func (c *Conn) SetContext(ctx context.Context) {
	c.ctx = ctx
}

func (c *Conn) Pair(authData ble.AuthData, to time.Duration) error {
	return c.smp.Pair(authData, to)
}

func (c *Conn) StartEncryption() error {
	return c.smp.StartEncryption()
}

// Read copies re-assembled L2CAP PDUs into sdu.
func (c *Conn) Read(sdu []byte) (n int, err error) {
	p, ok := <-c.chInPDU
	if !ok {
		return 0, errors.Wrap(io.ErrClosedPipe, "input channel closed")
	}
	if len(p) == 0 {
		return 0, errors.Wrap(io.ErrUnexpectedEOF, "received empty packet")
	}

	// Assume it's a B-Frame.
	slen := p.dlen()
	data := p.payload()
	if c.leFrame {
		// LE-Frame.
		slen = leFrameHdr(p).slen()
		data = leFrameHdr(p).payload()
	}
	if cap(sdu) < slen {
		return 0, errors.Wrapf(io.ErrShortBuffer, "payload received exceeds sdu buffer")
	}
	buf := bytes.NewBuffer(sdu)
	buf.Reset()
	buf.Write(data)
	for buf.Len() < slen {
		p := <-c.chInPDU
		buf.Write(p.payload())
	}
	return slen, nil
}

// Write breaks down a L2CAP SDU into segmants [Vol 3, Part A, 7.3.1]
func (c *Conn) Write(sdu []byte) (int, error) {
	if len(sdu) > c.txMTU {
		return 0, errors.Wrap(io.ErrShortWrite, "payload exceeds mtu")
	}

	plen := len(sdu)
	if plen > c.txMTU {
		plen = c.txMTU
	}
	b := make([]byte, 4+plen)
	binary.LittleEndian.PutUint16(b[0:2], uint16(len(sdu)))
	binary.LittleEndian.PutUint16(b[2:4], cidLEAtt)
	if c.leFrame {
		binary.LittleEndian.PutUint16(b[4:6], uint16(len(sdu)))
		copy(b[6:], sdu)
	} else {
		copy(b[4:], sdu)
	}
	sent, err := c.writePDU(b)
	if err != nil {
		return sent, err
	}
	sdu = sdu[plen:]

	for len(sdu) > 0 {
		plen := len(sdu)
		if plen > c.txMTU {
			plen = c.txMTU
		}
		n, err := c.writePDU(sdu[:plen])
		sent += n
		if err != nil {
			return sent, err
		}
		sdu = sdu[plen:]
	}
	return sent, nil
}

func (c *Conn) initPairingContext() {
	smp := c.smp

	la := c.LocalAddr().Bytes()
	lat := uint8(0x00)
	if (la[0] & 0xc0) == 0xc0 {
		lat = 0x01
	}
	ra := c.RemoteAddr().Bytes()
	rat := c.param.PeerAddressType()

	smp.InitContext(la, ra, lat, rat)
}

func (c *Conn) encrypt(bi hci.BondInfo) error {
	legacy, stk := c.smp.LegacyPairingInfo()
	//if a short term key is present, use it as the long term key
	if legacy && len(stk) > 0 {
		fmt.Println("encrypting with short term key")
		return c.stkEncrypt(stk)
	}

	if bi == nil {
		return fmt.Errorf("no bond information")
	}

	ltk := bi.LongTermKey()
	if ltk == nil {
		return fmt.Errorf("no ltk present")
	}

	m := cmd.LEStartEncryption{}
	m.ConnectionHandle = c.param.ConnectionHandle()

	eDiv := bi.EDiv()
	randVal := bi.Random()

	if bi.Legacy() {
		//expect LTK, EDiv, and Rand to be present
		if len(ltk) != 16 {
			return fmt.Errorf("invalid length for ltk")
		}

		if eDiv == 0 || randVal == 0 {
			return fmt.Errorf("ediv and random must not be 0 for legacy pairing")
		}
	}

	for i, v := range ltk {
		m.LongTermKey[i] = v
	}

	m.EncryptedDiversifier = eDiv
	m.RandomNumber = randVal

	return c.ctrl.Send(&m, nil)
}

func (c *Conn) stkEncrypt(key []byte) error {
	m := cmd.LEStartEncryption{}
	m.ConnectionHandle = c.param.ConnectionHandle()
	for i, v := range key {
		m.LongTermKey[i] = v
	}

	m.EncryptedDiversifier = 0
	m.RandomNumber = 0

	return c.ctrl.Send(&m, nil)
}

// writePDU breaks down a L2CAP PDU into fragments if it's larger than the HCI buffer size. [Vol 3, Part A, 7.2.1]
func (c *Conn) writePDU(pdu []byte) (int, error) {
	sent := 0
	flags := uint16(hci.PbfHostToControllerStart << 4) // ACL boundary flags

	// All L2CAP fragments associated with an L2CAP PDU shall be processed for
	// transmission by the Controller before any other L2CAP PDU for the same
	// logical transport shall be processed.
	c.txBuffer.Lock()
	defer c.txBuffer.Unlock()

	// Fail immediately if the connection is already closed
	// Check this with the pool locked to avoid race conditions
	// with handleDisconnectionComplete
	select {
	case <-c.chDone:
		return 0, io.ErrClosedPipe
	default:
	}

	for len(pdu) > 0 {
		// Get a buffer from our pre-allocated and flow-controlled pool.
		pkt := c.txBuffer.Get() // ACL pkt
		flen := len(pdu)        // fragment length
		if flen > pkt.Cap()-1-4 {
			flen = pkt.Cap() - 1 - 4
		}

		// Prepare the Headers

		// HCI Header: pkt Type
		if err := binary.Write(pkt, binary.LittleEndian, hci.PktTypeACLData); err != nil {
			return 0, err
		}
		// ACL Header: handle and flags
		if err := binary.Write(pkt, binary.LittleEndian, c.param.ConnectionHandle()|(flags<<8)); err != nil {
			return 0, err
		}
		// ACL Header: data len
		if err := binary.Write(pkt, binary.LittleEndian, uint16(flen)); err != nil {
			return 0, err
		}
		// Append payload
		if err := binary.Write(pkt, binary.LittleEndian, pdu[:flen]); err != nil {
			return 0, err
		}

		// Flush the pkt to HCI
		select {
		case <-c.chDone:
			return 0, io.ErrClosedPipe
		default:
		}

		if _, err := c.ctrl.SocketWrite(pkt.Bytes()); err != nil {
			return sent, err
		}
		sent += flen

		flags = hci.PbfContinuing << 4 // Set "continuing" in the boundary flags for the rest of fragments, if any.
		pdu = pdu[flen:]                  // Advance the point
	}
	return sent, nil
}

// Recombines fragments into a L2CAP PDU. [Vol 3, Part A, 7.2.2]
func (c *Conn) recombine() error {
	var pkt Packet
	var ok bool
	select {
	//todo: hci should cancel a parent context
	//case <-c.hci.done:
	//	fmt.Println("hci is done; return io.EOF")
	//	return io.EOF
	case pkt, ok = <-c.chInPkt:
		if !ok {
			fmt.Println("c.chInPkt is closed; return io.EOF")
			return io.EOF
		}
	case <-time.After(time.Minute * 10):
		fmt.Println("recombine timed out")
		return fmt.Errorf("idle timeout")
	case <-c.ctx.Done():
		return fmt.Errorf("connection cancelled: %s", c.ctx.Err())
	}

	p := Pdu(pkt.data())

	// Currently, check for LE-U only. For channels that we don't recognizes,
	// re-combine them anyway, and discard them later when we dispatch the PDU
	// according to CID.
	if p.cid() == cidLEAtt && p.dlen() > c.rxMPS {
		return fmt.Errorf("fragment size (%d) larger than rxMPS (%d)", p.dlen(), c.rxMPS)
	}

	// If this pkt is not a complete PDU, and we'll be receiving more
	// fragments, re-allocate the whole PDU (including Header).
	if len(p.payload()) < p.dlen() {
		p = make([]byte, 0, 4+p.dlen())
		p = append(p, Pdu(pkt.data())...)
	}
	for len(p) < 4+p.dlen() {
		if pkt, ok = <-c.chInPkt; !ok || (pkt.Pbf()&hci.PbfContinuing) == 0 {
			return io.ErrUnexpectedEOF
		}
		p = append(p, Pdu(pkt.data())...)
	}

	// TODO: support dynamic or assigned channels for LE-Frames.
	switch p.cid() {
	case cidLEAtt:
		c.chInPDU <- p
	case cidLESignal:
		_ = c.handleSignal(p)
	case CidSMP:
		_ = c.smp.Handle(p)
	default:
		//todo: change this back to a logger
		fmt.Println("recombine()", "unrecognized CID", fmt.Sprintf("%04X, [%X]", p.cid(), p))
	}
	return nil
}

// Disconnected returns a receiving channel, which is closed when the connection disconnects.
func (c *Conn) Disconnected() <-chan struct{} {
	return c.chDone
}

// Close disconnects the connection by sending hci disconnect command to the device.
func (c *Conn) Close() error {
	select {
	case <-c.chDone:
		// Return if it's already closed.
		return nil
	default:
		return c.ctrl.Send(&cmd.Disconnect{
			ConnectionHandle: c.param.ConnectionHandle(),
			Reason:           0x13,
		}, nil)
	}
}

// LocalAddr returns local device's MAC address.
func (c *Conn) LocalAddr() ble.Addr { return c.ctrl.Addr() }

// RemoteAddr returns remote device's MAC address.
func (c *Conn) RemoteAddr() ble.Addr {
	a := c.param.PeerAddress()
	return ble.NewAddr(net.HardwareAddr([]byte{a[5], a[4], a[3], a[2], a[1], a[0]}).String())
}

// RxMTU returns the MTU which the upper layer is capable of accepting.
func (c *Conn) RxMTU() int { return c.rxMTU }

// SetRxMTU sets the MTU which the upper layer is capable of accepting.
func (c *Conn) SetRxMTU(mtu int) { c.rxMTU, c.rxMPS = mtu, mtu }

// TxMTU returns the MTU which the remote device is capable of accepting.
func (c *Conn) TxMTU() int { return c.txMTU }

// SetTxMTU sets the MTU which the remote device is capable of accepting.
func (c *Conn) SetTxMTU(mtu int) { c.txMTU = mtu }

// pkt implements HCI ACL Data Packet [Vol 2, Part E, 5.4.2]
// Packet boundary flags , bit[5:6] of handle field's MSB
// Broadcast flags. bit[7:8] of handle field's MSB
// Not used in LE-U. Leave it as 0x00 (Point-to-Point).
// Broadcasting in LE uses ADVB logical transport.
type Packet []byte

func (a Packet) handle() uint16 { return uint16(a[0]) | (uint16(a[1]&0x0f) << 8) }
func (a Packet) Pbf() int       { return (int(a[1]) >> 4) & 0x3 }
func (a Packet) bcf() int       { return (int(a[1]) >> 6) & 0x3 }
func (a Packet) dlen() int      { return int(a[2]) | (int(a[3]) << 8) }
func (a Packet) data() []byte   { return a[4:] }

type Pdu []byte

func (p Pdu) dlen() int       { return int(binary.LittleEndian.Uint16(p[0:2])) }
func (p Pdu) cid() uint16     { return binary.LittleEndian.Uint16(p[2:4]) }
func (p Pdu) payload() []byte { return p[4:] }

type leFrameHdr Pdu

func (f leFrameHdr) slen() int       { return int(binary.LittleEndian.Uint16(f[4:6])) }
func (f leFrameHdr) payload() []byte { return f[6:] }
