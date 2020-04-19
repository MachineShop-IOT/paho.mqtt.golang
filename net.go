/*
 * Copyright (c) 2013 IBM Corp.
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v1.0
 * which accompanies this distribution, and is available at
 * http://www.eclipse.org/legal/epl-v10.html
 *
 * Contributors:
 *    Seth Hoenig
 *    Allan Stockdill-Mander
 *    Mike Robertson
 */

package mqtt

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"golang.org/x/net/proxy"
	"golang.org/x/net/websocket"
)

func signalError(c chan<- error, err error) {
	select {
	case c <- err:
	default:
	}
}

func openConnection(uri *url.URL, tlsc *tls.Config, timeout time.Duration, headers http.Header) (net.Conn, error) {
	switch uri.Scheme {
	case "ws":
		config, _ := websocket.NewConfig(uri.String(), fmt.Sprintf("http://%s", uri.Host))
		config.Protocol = []string{"mqtt"}
		config.Header = headers
		config.Dialer = &net.Dialer{Timeout: timeout}
		conn, err := websocket.DialConfig(config)
		if err != nil {
			return nil, err
		}
		conn.PayloadType = websocket.BinaryFrame
		return conn, err
	case "wss":
		config, _ := websocket.NewConfig(uri.String(), fmt.Sprintf("https://%s", uri.Host))
		config.Protocol = []string{"mqtt"}
		config.TlsConfig = tlsc
		config.Header = headers
		config.Dialer = &net.Dialer{Timeout: timeout}
		conn, err := websocket.DialConfig(config)
		if err != nil {
			return nil, err
		}
		conn.PayloadType = websocket.BinaryFrame
		return conn, err
	case "tcp":
		allProxy := os.Getenv("all_proxy")
		if len(allProxy) == 0 {
			conn, err := net.DialTimeout("tcp", uri.Host, timeout)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}
		proxyDialer := proxy.FromEnvironment()

		conn, err := proxyDialer.Dial("tcp", uri.Host)
		if err != nil {
			return nil, err
		}
		return conn, nil
	case "unix":
		conn, err := net.DialTimeout("unix", uri.Host, timeout)
		if err != nil {
			return nil, err
		}
		return conn, nil
	case "ssl":
		fallthrough
	case "tls":
		fallthrough
	case "tcps":
		allProxy := os.Getenv("all_proxy")
		if len(allProxy) == 0 {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", uri.Host, tlsc)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}
		proxyDialer := proxy.FromEnvironment()

		conn, err := proxyDialer.Dial("tcp", uri.Host)
		if err != nil {
			return nil, err
		}

		tlsConn := tls.Client(conn, tlsc)

		err = tlsConn.Handshake()
		if err != nil {
			conn.Close()
			return nil, err
		}

		return tlsConn, nil
	}
	return nil, errors.New("Unknown protocol")
}

// actually read incoming messages off the wire
// send Message object into ibound channel
func incoming(c *client) {
	var err error
	var cp packets.ControlPacket

	defer c.workers.Done()

	DEBUGD.Dumpln(*c.milieu, NET, "incoming started")

	for {
		if cp, err = packets.ReadPacket(c.conn); err != nil {
			break
		}
		DEBUGD.Dumpln(*c.milieu, NET, "Received Message")
		select {
		case c.ibound <- cp:
			// Notify keepalive logic that we recently received a packet
			if c.options.KeepAlive != 0 {
				c.lastReceived.Store(time.Now())
			}
		case <-c.stop:
			// This avoids a deadlock should a message arrive while shutting down.
			// In that case the "reader" of c.ibound might already be gone
			WARND.Dumpln(*c.milieu, NET, "incoming dropped a received message during shutdown")
			break
		}
	}
	// We received an error on read.
	// If disconnect is in progress, swallow error and return
	select {
	case <-c.stop:
		DEBUGD.Dumpln(*c.milieu, NET, "incoming stopped")
		return
	// Not trying to disconnect, send the error to the errors channel
	default:
		ERRORD.Dumpln(*c.milieu, NET, "incoming stopped with error", err)
		signalError(c.errors, err)
		return
	}
}

// receive a Message object on obound, and then
// actually send outgoing message to the wire
func outgoing(c *client) {
	defer c.workers.Done()
	DEBUGD.Dumpln(*c.milieu, NET, "outgoing started")

	for {
		DEBUGD.Dumpln(*c.milieu, NET, "outgoing waiting for an outbound message")
		select {
		case <-c.stop:
			DEBUGD.Dumpln(*c.milieu, NET, "outgoing stopped")
			return
		case pub := <-c.obound:
			msg := pub.p.(*packets.PublishPacket)

			if c.options.WriteTimeout > 0 {
				c.conn.SetWriteDeadline(time.Now().Add(c.options.WriteTimeout))
			}

			if err := msg.Write(c.conn); err != nil {
				ERRORD.Dumpln(*c.milieu, NET, "outgoing stopped with error", err)
				pub.t.setError(err)
				signalError(c.errors, err)
				return
			}

			if c.options.WriteTimeout > 0 {
				// If we successfully wrote, we don't want the timeout to happen during an idle period
				// so we reset it to infinite.
				c.conn.SetWriteDeadline(time.Time{})
			}

			if msg.Qos == 0 {
				pub.t.flowComplete()
			}
			DEBUGD.Dumpln(*c.milieu, NET, "obound wrote msg, id:", msg.MessageID)
		case msg := <-c.oboundP:
			switch msg.p.(type) {
			case *packets.SubscribePacket:
				msg.p.(*packets.SubscribePacket).MessageID = c.getID(msg.t)
			case *packets.UnsubscribePacket:
				msg.p.(*packets.UnsubscribePacket).MessageID = c.getID(msg.t)
			}
			DEBUGD.Dumpln(*c.milieu, NET, "obound priority msg to write, type", reflect.TypeOf(msg.p))
			if err := msg.p.Write(c.conn); err != nil {
				ERRORD.Dumpln(*c.milieu, NET, "outgoing stopped with error", err)
				if msg.t != nil {
					msg.t.setError(err)
				}
				signalError(c.errors, err)
				return
			}
			switch msg.p.(type) {
			case *packets.DisconnectPacket:
				msg.t.(*DisconnectToken).flowComplete()
				DEBUGD.Dumpln(*c.milieu, NET, "outbound wrote disconnect, stopping")
				return
			}
		}
		// Reset ping timer after sending control packet.
		if c.options.KeepAlive != 0 {
			c.lastSent.Store(time.Now())
		}
	}
}

// receive Message objects on ibound
// store messages if necessary
// send replies on obound
// delete messages from store if necessary
func alllogic(c *client) {
	defer c.workers.Done()
	DEBUGD.Dumpln(*c.milieu, NET, "logic started")

	for {
		DEBUGD.Dumpln(*c.milieu, NET, "logic waiting for msg on ibound")

		select {
		case msg := <-c.ibound:
			DEBUGD.Dumpln(*c.milieu, NET, "logic got msg on ibound")
			persistInbound(c.milieu, c.persist, msg)
			switch m := msg.(type) {
			case *packets.PingrespPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received pingresp")
				atomic.StoreInt32(&c.pingOutstanding, 0)
			case *packets.SubackPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received suback, id:", m.MessageID)
				token := c.getToken(m.MessageID)
				switch t := token.(type) {
				case *SubscribeToken:
					DEBUGD.Dumpln(*c.milieu, NET, "granted qoss", m.ReturnCodes)
					for i, qos := range m.ReturnCodes {
						t.subResult[t.subs[i]] = qos
					}
				}
				token.flowComplete()
				c.freeID(m.MessageID)
			case *packets.UnsubackPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received unsuback, id:", m.MessageID)
				c.getToken(m.MessageID).flowComplete()
				c.freeID(m.MessageID)
			case *packets.PublishPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received publish, msgId:", m.MessageID)
				DEBUGD.Dumpln(*c.milieu, NET, "putting msg on onPubChan")
				switch m.Qos {
				case 2:
					c.incomingPubChan <- m
					DEBUGD.Dumpln(*c.milieu, NET, "done putting msg on incomingPubChan")
				case 1:
					c.incomingPubChan <- m
					DEBUGD.Dumpln(*c.milieu, NET, "done putting msg on incomingPubChan")
				case 0:
					select {
					case c.incomingPubChan <- m:
					case <-c.stop:
					}
					DEBUGD.Dumpln(*c.milieu, NET, "done putting msg on incomingPubChan")
				}
			case *packets.PubackPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received puback, id:", m.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				c.getToken(m.MessageID).flowComplete()
				c.freeID(m.MessageID)
			case *packets.PubrecPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received pubrec, id:", m.MessageID)
				prel := packets.NewControlPacket(packets.Pubrel).(*packets.PubrelPacket)
				prel.MessageID = m.MessageID
				select {
				case c.oboundP <- &PacketAndToken{p: prel, t: nil}:
				case <-c.stop:
				}
			case *packets.PubrelPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received pubrel, id:", m.MessageID)
				pc := packets.NewControlPacket(packets.Pubcomp).(*packets.PubcompPacket)
				pc.MessageID = m.MessageID
				persistOutbound(c.milieu, c.persist, pc)
				select {
				case c.oboundP <- &PacketAndToken{p: pc, t: nil}:
				case <-c.stop:
				}
			case *packets.PubcompPacket:
				DEBUGD.Dumpln(*c.milieu, NET, "received pubcomp, id:", m.MessageID)
				c.getToken(m.MessageID).flowComplete()
				c.freeID(m.MessageID)
			}
		case <-c.stop:
			WARND.Dumpln(*c.milieu, NET, "logic stopped")
			return
		}
	}
}

func (c *client) ackFunc(packet *packets.PublishPacket) func() {
	return func() {
		switch packet.Qos {
		case 2:
			pr := packets.NewControlPacket(packets.Pubrec).(*packets.PubrecPacket)
			pr.MessageID = packet.MessageID
			DEBUGD.Dumpln(*c.milieu, NET, "putting pubrec msg on obound")
			select {
			case c.oboundP <- &PacketAndToken{p: pr, t: nil}:
			case <-c.stop:
			}
			DEBUGD.Dumpln(*c.milieu, NET, "done putting pubrec msg on obound")
		case 1:
			pa := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
			pa.MessageID = packet.MessageID
			DEBUGD.Dumpln(*c.milieu, NET, "putting puback msg on obound")
			persistOutbound(c.milieu, c.persist, pa)
			select {
			case c.oboundP <- &PacketAndToken{p: pa, t: nil}:
			case <-c.stop:
			}
			DEBUGD.Dumpln(*c.milieu, NET, "done putting puback msg on obound")
		case 0:
			// do nothing, since there is no need to send an ack packet back
		}
	}
}

func errorWatch(c *client) {
	defer c.workers.Done()
	select {
	case <-c.stop:
		WARND.Dumpln(*c.milieu, NET, "errorWatch stopped")
		return
	case err := <-c.errors:
		ERRORD.Dumpln(*c.milieu, NET, "error triggered, stopping")
		go c.internalConnLost(err)
		return
	}
}
