// Package bacip implements a Bacnet/IP client
package bacip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/REQUEA/bacnet"
)

type Client struct {
	//Maybe change to bacnet address
	ipAddress        net.IP
	broadcastAddress net.IP
	udpPort          int
	udp              *net.UDPConn
	subscriptions    *Subscriptions
	transactions     *Transactions
	logger           Logger
	runFlag          atomic.Bool
	wg               sync.WaitGroup
}

type Logger interface {
	Info(...interface{})
	Error(...interface{})
}

type NoOpLogger struct{}

func (NoOpLogger) Info(...interface{})  {}
func (NoOpLogger) Error(...interface{}) {}

type Subscriptions struct {
	sync.RWMutex
	f func(BVLC, net.UDPAddr)
}

const DefaultUDPPort = 47808

func broadcastAddr(n *net.IPNet) (net.IP, error) {
	if n.IP.To4() == nil {
		return net.IP{}, errors.New("does not support IPv6 addresses")
	}
	ip := make(net.IP, len(n.IP.To4()))

	binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(n.IP.To4())|^binary.BigEndian.Uint32(net.IP(n.Mask).To4()))
	return ip, nil
}

// NewClient creates a new bacnet client. It binds on the given port
// and network interface or cidr addr. If Port is 0, a random port is used
func NewClient(netInterface string, port int, logger Logger) (*Client, error) {
	c := &Client{subscriptions: &Subscriptions{},
		transactions: NewTransactions(),
		logger:       logger,
		runFlag:      atomic.Bool{},
		wg:           sync.WaitGroup{},
	}
	if strings.Contains(netInterface, "/") {
		c.tryParse(netInterface)
	} else {
		i, err := net.InterfaceByName(netInterface)
		if err != nil {
			return nil, fmt.Errorf("interface %s: %w", netInterface, err)
		}
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("interface %s has no addresses", netInterface)
		}
		for _, adr := range addrs {
			if c.tryParse(adr.String()) {
				break
			}
		}
	}
	if c.ipAddress == nil {
		return nil, fmt.Errorf("no IPv4 address assigned to interface %s", netInterface)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: port,
	})
	if err != nil {
		return nil, err
	}
	c.runFlag.Store(true)
	c.udpPort = conn.LocalAddr().(*net.UDPAddr).Port
	c.udp = conn
	c.wg.Add(1)
	go c.listen()
	return c, nil
}

func (c *Client) tryParse(cidr string) bool {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	// To4 is nil when type is ip6
	if ip.To4() != nil {
		broadcast, err := broadcastAddr(ipnet)
		if err != nil {
			return false
		}
		c.ipAddress = ip.To4()
		c.broadcastAddress = broadcast
		return true
	}
	return false
}

// listen for incoming bacnet packets.
func (c *Client) listen() {
	defer c.wg.Done()
	for c.runFlag.Load() {
		b := make([]byte, 2048)
		i, addr, err := c.udp.ReadFromUDP(b)
		if err != nil {
			c.logger.Error(err.Error())
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("panic in handle message: ", r)
				}
			}()
			err := c.handleMessage(addr, b[:i])
			if err != nil {
				c.logger.Error("handle msg: ", err)
			}
		}()
	}
}

func (c *Client) Close() error {
	c.runFlag.Store(false)
	c.wg.Wait()
	return c.udp.Close()
}

func (c *Client) handleMessage(src *net.UDPAddr, b []byte) error {
	var bvlc BVLC
	err := bvlc.UnmarshalBinary(b)
	if err != nil && errors.Is(err, ErrNotBAcnetIP) {
		return err
	}
	apdu := bvlc.NPDU.ADPU
	if apdu == nil {
		c.logger.Info(fmt.Sprintf("Received network packet %+v", bvlc.NPDU))
		return nil
	}
	c.subscriptions.RLock()
	if c.subscriptions.f != nil {
		//If f block, there is a deadlock here
		c.subscriptions.f(bvlc, *src)
	}
	c.subscriptions.RUnlock()
	if apdu.DataType == ComplexAck || apdu.DataType == SimpleAck || apdu.DataType == Error {
		invokeID := bvlc.NPDU.ADPU.InvokeID
		tx, ok := c.transactions.GetTransaction(invokeID)
		if !ok {
			return fmt.Errorf("no transaction found for id %d", invokeID)
		}
		select {
		case tx.APDU <- *apdu:
			return nil
		case <-tx.Ctx.Done():
			return fmt.Errorf("handler for tx %d: %w", invokeID, tx.Ctx.Err())
		}
	}
	return nil
}

func (c *Client) WhoIs(data WhoIs, timeout time.Duration) ([]bacnet.Device, error) {
	npdu := NPDU{
		Version:               Version1,
		IsNetworkLayerMessage: false,
		ExpectingReply:        false,
		Priority:              Normal,
		Destination:           nil,
		Source:                nil,
		ADPU: &APDU{
			DataType:    UnconfirmedServiceRequest,
			ServiceType: ServiceUnconfirmedWhoIs,
			Payload:     &data,
		},
	}

	rChan := make(chan struct {
		bvlc BVLC
		src  net.UDPAddr
	})
	c.subscriptions.Lock()
	//TODO:  add errgroup ?, ensure all f are done and not blocked
	c.subscriptions.f = func(bvlc BVLC, src net.UDPAddr) {
		rChan <- struct {
			bvlc BVLC
			src  net.UDPAddr
		}{
			bvlc: bvlc,
			src:  src,
		}
	}
	c.subscriptions.Unlock()
	defer func() {
		c.subscriptions.f = nil
	}()
	_, err := c.broadcast(npdu)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	//Use a set to deduplicate results
	set := map[Iam]bacnet.Address{}
	for {
		select {
		case <-timer.C:
			result := []bacnet.Device{}
			for iam, addr := range set {
				result = append(result, bacnet.Device{
					ID:           iam.ObjectID,
					MaxApdu:      iam.MaxApduLength,
					Segmentation: iam.SegmentationSupport,
					Vendor:       iam.VendorID,
					Addr:         addr,
				})
			}
			return result, nil
		case r := <-rChan:
			//clean/filter  network answers here
			apdu := r.bvlc.NPDU.ADPU
			if apdu != nil {
				if apdu.DataType == UnconfirmedServiceRequest &&
					apdu.ServiceType == ServiceUnconfirmedIAm {
					iam, ok := apdu.Payload.(*Iam)
					if !ok {
						return nil, fmt.Errorf("unexpected payload type %T", apdu.Payload)
					}
					//Only add a result that we are interested in. Well-
					//behaved devices should not answer if their
					//InstanceID isn't in the given range. But because
					//the IAM response is in broadcast mode, we might
					//receive an answer triggered by another whois
					if data.High != nil && data.Low != nil {
						if iam.ObjectID.Instance >= bacnet.ObjectInstance(*data.Low) &&
							iam.ObjectID.Instance <= bacnet.ObjectInstance(*data.High) {
							addr := bacnet.AddressFromUDP(r.src)
							set[*iam] = *addr
						}
					} else {
						addr := bacnet.AddressFromUDP(r.src)
						set[*iam] = *addr
					}

				}
			}
		}
	}
}

func (c *Client) ReadProperty(ctx context.Context, device bacnet.Device, readProp ReadProperty) (interface{}, error) {
	invokeID := c.transactions.GetID()
	defer c.transactions.FreeID(invokeID)
	npdu := NPDU{
		Version:               Version1,
		IsNetworkLayerMessage: false,
		ExpectingReply:        true,
		Priority:              Normal,
		Destination:           &device.Addr,
		Source: bacnet.AddressFromUDP(net.UDPAddr{
			IP:   c.ipAddress,
			Port: c.udpPort,
		}),
		HopCount: 255,
		ADPU: &APDU{
			DataType:    ConfirmedServiceRequest,
			ServiceType: ServiceConfirmedReadProperty,
			InvokeID:    invokeID,
			Payload:     &readProp,
		},
	}
	rChan := make(chan APDU)
	c.transactions.SetTransaction(invokeID, rChan, ctx)
	defer c.transactions.StopTransaction(invokeID)
	_, err := c.send(npdu)
	if err != nil {
		return nil, err
	}
	select {
	case apdu := <-rChan:
		//Todo: ensure response validity, ensure conversion cannot panic
		if apdu.DataType == Error {
			return nil, *apdu.Payload.(*ApduError)
		}
		if apdu.DataType == ComplexAck && apdu.ServiceType == ServiceConfirmedReadProperty {
			data := apdu.Payload.(*ReadProperty).Data
			return data, nil
		}
		return nil, errors.New("invalid answer")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) WriteProperty(ctx context.Context, device bacnet.Device, writeProp WriteProperty) error {
	invokeID := c.transactions.GetID()
	defer c.transactions.FreeID(invokeID)
	npdu := NPDU{
		Version:               Version1,
		IsNetworkLayerMessage: false,
		ExpectingReply:        true,
		Priority:              Normal,
		Destination:           &device.Addr,
		Source: bacnet.AddressFromUDP(net.UDPAddr{
			IP:   c.ipAddress,
			Port: c.udpPort,
		}),
		HopCount: 255,
		ADPU: &APDU{
			DataType:    ConfirmedServiceRequest,
			ServiceType: ServiceConfirmedWriteProperty,
			InvokeID:    invokeID,
			Payload:     &writeProp,
		},
	}
	rChan := make(chan APDU)
	c.transactions.SetTransaction(invokeID, rChan, ctx)
	defer c.transactions.StopTransaction(invokeID)
	_, err := c.send(npdu)
	if err != nil {
		return err
	}
	select {
	case apdu := <-rChan:
		//Todo: ensure response validity, ensure conversion cannot panic
		if apdu.DataType == Error {
			return *apdu.Payload.(*ApduError)
		}
		if apdu.DataType == SimpleAck {
			return nil
		}
		return errors.New("invalid answer")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) send(npdu NPDU) (int, error) {
	bytes, err := BVLC{
		Type:     TypeBacnetIP,
		Function: BacFuncUnicast,
		NPDU:     npdu,
	}.MarshalBinary()
	if err != nil {
		return 0, err
	}
	if npdu.Destination == nil {
		return 0, fmt.Errorf("destination bacnet address should be not nil to send unicast")
	}
	addr := bacnet.UDPFromAddress(*npdu.Destination)
	return c.udp.WriteToUDP(bytes, &addr)

}

func (c *Client) broadcast(npdu NPDU) (int, error) {
	bytes, err := BVLC{
		Type:     TypeBacnetIP,
		Function: BacFuncBroadcast,
		NPDU:     npdu,
	}.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return c.udp.WriteToUDP(bytes, &net.UDPAddr{
		IP:   c.broadcastAddress,
		Port: DefaultUDPPort,
	})
}
