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

type (
	// Logger interface allows implementations to provide to this package any
	// object that implements the methods defined in it.
	Logger interface {
		Println(v ...interface{})
		Printf(format string, v ...interface{})
	}

	// NOOPLogger implements the logger that does not perform any operation
	// by default. This allows us to efficiently discard the unwanted messages.
	NOOPLogger struct{}

	Debugger interface {
		Dumpln(m Milieu, v ...interface{})
		Dumpf(m Milieu, format string, v ...interface{})
	}
	Milieu struct {
		Broker string
		Client
	}

	PassthroughDebugger struct {
		receiver *Logger
	}
)

func (NOOPLogger) Println(v ...interface{})               {}
func (NOOPLogger) Printf(format string, v ...interface{}) {}

func (p PassthroughDebugger) Dumpln(m Milieu, v ...interface{}) {
	deref := *p.receiver
	deref.Println(v...)
}
func (p PassthroughDebugger) Dumpf(m Milieu, format string, v ...interface{}) {
	deref := *p.receiver
	deref.Printf(format, v...)
}

// Internal levels of library output that are initialised to not print
// anything but can be overridden by programmer
var (
	ERROR    Logger = NOOPLogger{}
	CRITICAL Logger = NOOPLogger{}
	WARN     Logger = NOOPLogger{}
	DEBUG    Logger = NOOPLogger{}

	ERRORD    Debugger = PassthroughDebugger{&ERROR}
	CRITICALD Debugger = PassthroughDebugger{&CRITICAL}
	WARND     Debugger = PassthroughDebugger{&WARN}
	DEBUGD    Debugger = PassthroughDebugger{&DEBUG}
)
