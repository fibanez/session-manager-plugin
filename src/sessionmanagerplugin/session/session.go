// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package session starts the session.
package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"session-manager-plugin/src/config"
	"session-manager-plugin/src/datachannel"
	"session-manager-plugin/src/jsonutil"
	"session-manager-plugin/src/log"
	"session-manager-plugin/src/message"
	"session-manager-plugin/src/retry"
	"session-manager-plugin/src/sdkutil"
	"session-manager-plugin/src/sessionmanagerplugin/session/sessionutil"
	"session-manager-plugin/src/version"

	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/twinj/uuid"
	"github.com/xtaci/smux"
	"golang.org/x/sync/errgroup"
)

const (
	LegacyArgumentLength  = 4
	ArgumentLength        = 7
	StartSessionOperation = "StartSession"
	VersionFile           = "VERSION"
)

type ISession interface {
	Execute(log.T) error
	OpenDataChannel(log.T) error
	ProcessFirstMessage(log log.T, outputMessage message.ClientMessage) (isHandlerReady bool, err error)
	Stop()
	GetResumeSessionParams(log.T) (string, error)
	ResumeSessionHandler(log.T) error
	TerminateSession(log.T) error
}

const (
	LocalPortForwardingType = "LocalPortForwarding"
)

type PortSession struct {
	Session
	portParameters  PortParameters
	portSessionType IPortSession
}

type IPortSession interface {
	IsStreamNotSet() (status bool)
	InitializeStreams(log log.T) (err error)
	ReadStream(log log.T) (err error)
	WriteStream(outputMessage message.ClientMessage) (err error)
	Stop()
}

type PortParameters struct {
	PortNumber          string `json:"portNumber"`
	LocalPortNumber     string `json:"localPortNumber"`
	LocalUnixSocket     string `json:"localUnixSocket"`
	LocalConnectionType string `json:"localConnectionType"`
	Type                string `json:"type"`
}

type Session struct {
	DataChannel           datachannel.IDataChannel
	SessionId             string
	StreamUrl             string
	TokenValue            string
	IsAwsCliUpgradeNeeded bool
	Endpoint              string
	ClientId              string
	TargetId              string
	sdk                   *ssm.SSM
	retryParams           retry.RepeatableExponentialRetryer
	SessionType           string
	SessionProperties     interface{}
	DisplayMode           sessionutil.DisplayMode
}

//startSession create the datachannel for session
var startSession = func(session *Session, log log.T) error {
	return session.Execute(log)
}

//setSessionHandlersWithSessionType set session handlers based on session subtype
func setSessionHandlersWithSessionType(session *Session, log log.T) error {
	// SessionType is set inside DataChannel
	//sessionSubType := SessionRegistry[session.SessionType]
	//sessionSubType.Initialize(log, session)
	//return sessionSubType.SetSessionHandlers(log)

	portSessionSubType := &PortSession{}
	portSessionSubType.Initialize(log, session)
	return portSessionSubType.SetSessionHandlers(log)
}

// ValidateInputAndStartSession validates input sent from AWS CLI and starts a session if validation is successful.
// AWS CLI sends input in the order of
// args[0] will be path of executable (ignored)
// args[1] is session response
// args[2] is client region
// args[3] is operation name
// args[4] is profile name from aws credentials/config files
// args[5] is parameters input to aws cli for StartSession api
// args[6] is endpoint for ssm service
func ValidateInputAndStartSession(args []string, out io.Writer) {
	var (
		err                error
		session            Session
		startSessionOutput ssm.StartSessionOutput
		response           []byte
		region             string
		operationName      string
		profile            string
		ssmEndpoint        string
		target             string
	)
	log := log.Logger(true, "session-manager-plugin")
	uuid.SwitchFormat(uuid.CleanHyphen)

	if len(args) == 1 {
		fmt.Fprint(out, "\nThe Session Manager plugin was installed successfully. "+
			"Use the AWS CLI to start a session.\n\n")
		return
	} else if len(args) == 2 && args[1] == "--version" {
		fmt.Fprintf(out, "%s\n", string(version.Version))
		return
	} else if len(args) >= 2 && len(args) < LegacyArgumentLength {
		fmt.Fprintf(out, "\nUnknown operation %s. \nUse "+
			"session-manager-plugin --version to check the version.\n\n", string(args[1]))
		return

	} else if len(args) == LegacyArgumentLength {
		// If arguments do not have Profile passed from AWS CLI to Session-Manager-Plugin then
		// should be upgraded to use Session Manager encryption feature
		session.IsAwsCliUpgradeNeeded = true
	}

	for argsIndex := 1; argsIndex < len(args); argsIndex++ {
		switch argsIndex {
		case 1:
			response = []byte(args[1])
		case 2:
			region = args[2]
		case 3:
			operationName = args[3]
		case 4:
			profile = args[4]
		case 5:
			// args[5] is parameters input to aws cli for StartSession api call
			startSessionRequest := make(map[string]interface{})
			json.Unmarshal([]byte(args[5]), &startSessionRequest)
			target = startSessionRequest["Target"].(string)
		case 6:
			ssmEndpoint = args[6]
		}
	}
	sdkutil.SetRegionAndProfile(region, profile)
	clientId := uuid.NewV4().String()

	switch operationName {
	case StartSessionOperation:
		if err = json.Unmarshal(response, &startSessionOutput); err != nil {
			log.Errorf("Cannot perform start session: %v", err)
			fmt.Fprintf(out, "Cannot perform start session: %v\n", err)
			return
		}

		session.SessionId = *startSessionOutput.SessionId
		session.StreamUrl = *startSessionOutput.StreamUrl
		session.TokenValue = *startSessionOutput.TokenValue
		session.Endpoint = ssmEndpoint
		session.ClientId = clientId
		session.TargetId = target
		session.DataChannel = &datachannel.DataChannel{}

	default:
		fmt.Fprint(out, "Invalid Operation")
		return
	}

	if err = startSession(&session, log); err != nil {
		log.Errorf("Cannot perform start session: %v", err)
		fmt.Fprintf(out, "Cannot perform start session: %v\n", err)
		return
	}
}

//Execute create data channel and start the session
func (s *Session) Execute(log log.T) (err error) {
	fmt.Fprintf(os.Stdout, "\nStarting session with SessionId: %s\n", s.SessionId)

	// sets the display mode
	s.DisplayMode = sessionutil.NewDisplayMode(log)

	if err = s.OpenDataChannel(log); err != nil {
		log.Errorf("Error in Opening data channel: %v", err)
		return
	}

	// The session type is set either by handshake or the first packet received.
	if !<-s.DataChannel.IsSessionTypeSet() {
		log.Errorf("unable to SessionType for session %s", s.SessionId)
		return errors.New("unable to determine SessionType")
	} else {
		s.SessionType = s.DataChannel.GetSessionType()
		fmt.Fprintf(os.Stdout, "\nSessionType. %s\n", s.SessionType)
		s.SessionProperties = s.DataChannel.GetSessionProperties()
		fmt.Fprintf(os.Stdout, "\nSessionProperties. %s\n", s.SessionProperties)
		if err = setSessionHandlersWithSessionType(s, log); err != nil {
			log.Errorf("Session ending with error: %v", err)
			return
		}
	}
	return
}

func (s *PortSession) Initialize(log log.T, sessionVar *Session) {
	s.Session = *sessionVar
	if err := jsonutil.Remarshal(s.SessionProperties, &s.portParameters); err != nil {
		log.Errorf("Invalid format: %v", err)
	}

	if s.portParameters.Type == LocalPortForwardingType {
		if version.DoesAgentSupportTCPMultiplexing(log, s.DataChannel.GetAgentVersion()) {
			s.portSessionType = &MuxPortForwarding{
				sessionId:      s.SessionId,
				portParameters: s.portParameters,
				session:        s.Session,
			}
		} else {
			s.portSessionType = &BasicPortForwarding{
				sessionId:      s.SessionId,
				portParameters: s.portParameters,
				session:        s.Session,
			}
		}
	} else {
		s.portSessionType = &StandardStreamForwarding{
			portParameters: s.portParameters,
			session:        s.Session,
		}
	}

	s.DataChannel.RegisterOutputStreamHandler(s.ProcessStreamMessagePayload, true)
	s.DataChannel.GetWsChannel().SetOnMessage(func(input []byte) {
		if s.portSessionType.IsStreamNotSet() {
			outputMessage := &message.ClientMessage{}
			if err := outputMessage.DeserializeClientMessage(log, input); err != nil {
				log.Debugf("Ignore message deserialize error while stream connection had not set.")
				return
			}
			if outputMessage.MessageType == message.OutputStreamMessage {
				log.Debugf("Waiting for user to establish connection before processing incoming messages.")
				return
			} else {
				log.Infof("Received %s message while establishing connection", outputMessage.MessageType)
			}
		}
		s.DataChannel.OutputMessageHandler(log, s.Stop, s.SessionId, input)
	})
	log.Infof("Connected to instance[%s] on port: %s", sessionVar.TargetId, s.portParameters.PortNumber)
}

func (s *PortSession) Stop() {
	s.portSessionType.Stop()
}

// StartSession redirects inputStream/outputStream data to datachannel.
func (s *PortSession) SetSessionHandlers(log log.T) (err error) {
	if err = s.portSessionType.InitializeStreams(log); err != nil {
		return err
	}

	if err = s.portSessionType.ReadStream(log); err != nil {
		return err
	}
	return
}

// ProcessStreamMessagePayload writes messages received on datachannel to stdout
func (s *PortSession) ProcessStreamMessagePayload(log log.T, outputMessage message.ClientMessage) (isHandlerReady bool, err error) {
	if s.portSessionType.IsStreamNotSet() {
		log.Debugf("Waiting for streams to be established before processing incoming messages.")
		return false, nil
	}
	log.Tracef("Received payload of size %d from datachannel.", outputMessage.PayloadLength)
	err = s.portSessionType.WriteStream(outputMessage)
	return true, err
}

/**************** MuxPortForwarding ********************************/

// MuxClient contains smux client session and corresponding network connection
type MuxClient struct {
	conn    net.Conn
	session *smux.Session
}

// MgsConn contains local server and corresponding connection to smux client
type MgsConn struct {
	listener net.Listener
	conn     net.Conn
}

// MuxPortForwarding is type of port session
// accepts multiple client connections through multiplexing
type MuxPortForwarding struct {
	port           IPortSession
	sessionId      string
	socketFile     string
	portParameters PortParameters
	session        Session
	muxClient      *MuxClient
	mgsConn        *MgsConn
}

func (c *MgsConn) close() {
	c.listener.Close()
	c.conn.Close()
}

func (c *MuxClient) close() {
	c.session.Close()
	c.conn.Close()
}

// IsStreamNotSet checks if stream is not set
func (p *MuxPortForwarding) IsStreamNotSet() (status bool) {
	return p.muxClient.conn == nil
}

// Stop closes all open stream
func (p *MuxPortForwarding) Stop() {
	if p.mgsConn != nil {
		p.mgsConn.close()
	}
	if p.muxClient != nil {
		p.muxClient.close()
	}
	p.cleanUp()
	os.Exit(0)
}

// InitializeStreams initializes i/o streams
func (p *MuxPortForwarding) InitializeStreams(log log.T) (err error) {

	p.handleControlSignals(log)
	p.socketFile = getUnixSocketPath(p.sessionId, os.TempDir(), "session_manager_plugin_mux.sock")

	if err = p.initialize(log); err != nil {
		p.cleanUp()
	}
	return
}

// ReadStream reads data from different connections
func (p *MuxPortForwarding) ReadStream(log log.T) (err error) {
	g, ctx := errgroup.WithContext(context.Background())

	// reads data from smux client and transfers to server over datachannel
	g.Go(func() error {
		return p.transferDataToServer(log, ctx)
	})

	// set up network listener on SSM port and handle client connections
	g.Go(func() error {
		return p.handleClientConnections(log, ctx)
	})

	return g.Wait()
}

// WriteStream writes data to stream
func (p *MuxPortForwarding) WriteStream(outputMessage message.ClientMessage) error {
	switch message.PayloadType(outputMessage.PayloadType) {
	case message.Output:
		_, err := p.mgsConn.conn.Write(outputMessage.Payload)
		return err
	case message.Flag:
		var flag message.PayloadTypeFlag
		buf := bytes.NewBuffer(outputMessage.Payload)
		binary.Read(buf, binary.BigEndian, &flag)

		if message.ConnectToPortError == flag {
			fmt.Printf("\nConnection to destination port failed, check SSM Agent logs.\n")
		}
	}
	return nil
}

// cleanUp deletes unix socket file
func (p *MuxPortForwarding) cleanUp() {
	os.Remove(p.socketFile)
}

// initialize opens a network connection that acts as smux client
func (p *MuxPortForwarding) initialize(log log.T) (err error) {

	// open a network listener
	var listener net.Listener
	if listener, err = sessionutil.NewListener(log, p.socketFile); err != nil {
		return
	}

	var g errgroup.Group
	// start a go routine to accept connections on the network listener
	g.Go(func() error {
		if conn, err := listener.Accept(); err != nil {
			return err
		} else {
			p.mgsConn = &MgsConn{listener, conn}
		}
		return nil
	})

	// start a connection to the local network listener and set up client side of mux
	g.Go(func() error {
		if muxConn, err := net.Dial(listener.Addr().Network(), listener.Addr().String()); err != nil {
			return err
		} else if muxSession, err := smux.Client(muxConn, nil); err != nil {
			return err
		} else {
			p.muxClient = &MuxClient{muxConn, muxSession}
		}
		return nil
	})

	return g.Wait()
}

// handleControlSignals handles terminate signals
func (p *MuxPortForwarding) handleControlSignals(log log.T) {
	c := make(chan os.Signal)
	signal.Notify(c, sessionutil.ControlSignals...)
	go func() {
		<-c
		fmt.Println("Terminate signal received, exiting.")

		if err := p.session.DataChannel.SendFlag(log, message.TerminateSession); err != nil {
			log.Errorf("Failed to send TerminateSession flag: %v", err)
		}
		fmt.Fprintf(os.Stdout, "\n\nExiting session with sessionId: %s.\n\n", p.sessionId)
		p.Stop()
	}()
}

// transferDataToServer reads from smux client connection and sends on data channel
func (p *MuxPortForwarding) transferDataToServer(log log.T, ctx context.Context) (err error) {
	msg := make([]byte, config.StreamDataPayloadSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			var numBytes int
			if numBytes, err = p.mgsConn.conn.Read(msg); err != nil {
				log.Debugf("Reading from port failed with error: %v.", err)
				return
			}

			log.Tracef("Received message of size %d from mux client.", numBytes)
			if err = p.session.DataChannel.SendInputDataMessage(log, message.Output, msg[:numBytes]); err != nil {
				log.Errorf("Failed to send packet on data channel: %v", err)
				return
			}
			// sleep to process more data
			time.Sleep(time.Millisecond)
		}
	}
}

// handleClientConnections sets up network server on local ssm port to accept connections from clients (browser/terminal)
func (p *MuxPortForwarding) handleClientConnections(log log.T, ctx context.Context) (err error) {
	var (
		listener   net.Listener
		displayMsg string
	)

	if p.portParameters.LocalConnectionType == "unix" {
		if listener, err = net.Listen(p.portParameters.LocalConnectionType, p.portParameters.LocalUnixSocket); err != nil {
			return err
		}
		displayMsg = fmt.Sprintf("Unix socket %s opened for sessionId %s.", p.portParameters.LocalUnixSocket, p.sessionId)
	} else {
		localPortNumber := p.portParameters.LocalPortNumber
		if p.portParameters.LocalPortNumber == "" {
			localPortNumber = "0"
		}
		if listener, err = net.Listen("tcp", "localhost:"+localPortNumber); err != nil {
			return err
		}
		p.portParameters.LocalPortNumber = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		displayMsg = fmt.Sprintf("Port %s opened for sessionId %s.", p.portParameters.LocalPortNumber, p.sessionId)
	}

	defer listener.Close()

	log.Infof(displayMsg)
	fmt.Printf(displayMsg)

	// FI
	//log.Infof("Waiting for connections...\n")
	//fmt.Printf("\nWaiting for connections...\n")

	var once sync.Once
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if conn, err := listener.Accept(); err != nil {
				log.Errorf("Error while accepting connection: %v", err)
			} else {
				log.Infof("Connection accepted from %s\n for session [%s]", conn.RemoteAddr(), p.sessionId)

				once.Do(func() {
					fmt.Printf("\nConnection accepted for session [%s]\n", p.sessionId)
				})

				stream, err := p.muxClient.session.OpenStream()
				if err != nil {
					continue
				}
				log.Debugf("Client stream opened %d\n", stream.ID())
				go handleDataTransfer(stream, conn)
			}
		}
	}
}

// handleDataTransfer launches routines to transfer data between source and destination
func handleDataTransfer(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
	var wait sync.WaitGroup
	wait.Add(2)

	go func() {
		io.Copy(dst, src)
		dst.Close()
		wait.Done()
	}()

	go func() {
		io.Copy(src, dst)
		src.Close()
		wait.Done()
	}()

	wait.Wait()
}

// getUnixSocketPath generates the unix socket file name based on sessionId and returns the path.
func getUnixSocketPath(sessionId string, dir string, suffix string) string {
	hash := fnv.New32a()
	hash.Write([]byte(sessionId))
	return filepath.Join(dir, fmt.Sprintf("%d_%s", hash.Sum32(), suffix))
}

/**************** MuxPortForwarding ********************************/

/**************** BasicortForwarding ********************************/
// BasicPortForwarding is type of port session
// accepts one client connection at a time
type BasicPortForwarding struct {
	port           IPortSession
	stream         *net.Conn
	listener       *net.Listener
	sessionId      string
	portParameters PortParameters
	session        Session
}

// getNewListener returns a new listener to given address and type like tcp, unix etc.
var getNewListener = func(listenerType string, listenerAddress string) (listener net.Listener, err error) {
	return net.Listen(listenerType, listenerAddress)
}

// acceptConnection returns connection to the listener
var acceptConnection = func(log log.T, listener net.Listener) (tcpConn net.Conn, err error) {
	return listener.Accept()
}

// IsStreamNotSet checks if stream is not set
func (p *BasicPortForwarding) IsStreamNotSet() (status bool) {
	return p.stream == nil
}

// Stop closes the stream
func (p *BasicPortForwarding) Stop() {
	if p.stream != nil {
		(*p.stream).Close()
	}
	os.Exit(0)
}

// InitializeStreams establishes connection and initializes the stream
func (p *BasicPortForwarding) InitializeStreams(log log.T) (err error) {
	p.handleControlSignals(log)
	if err = p.startLocalConn(log); err != nil {
		return
	}
	return
}

// ReadStream reads data from the stream
func (p *BasicPortForwarding) ReadStream(log log.T) (err error) {
	msg := make([]byte, config.StreamDataPayloadSize)
	for {
		numBytes, err := (*p.stream).Read(msg)
		if err != nil {
			log.Debugf("Reading from port %s failed with error: %v. Close this connection, listen and accept new one.",
				p.portParameters.PortNumber, err)

			// Send DisconnectToPort flag to agent when client tcp connection drops to ensure agent closes tcp connection too with server port
			if err = p.session.DataChannel.SendFlag(log, message.DisconnectToPort); err != nil {
				log.Errorf("Failed to send packet: %v", err)
				return err
			}

			if err = p.reconnect(log); err != nil {
				return err
			}

			// continue to read from connection as it has been re-established
			continue
		}

		log.Tracef("Received message of size %d from stdin.", numBytes)
		if err = p.session.DataChannel.SendInputDataMessage(log, message.Output, msg[:numBytes]); err != nil {
			log.Errorf("Failed to send packet: %v", err)
			return err
		}
		// Sleep to process more data
		time.Sleep(time.Millisecond)
	}
}

// WriteStream writes data to stream
func (p *BasicPortForwarding) WriteStream(outputMessage message.ClientMessage) error {
	_, err := (*p.stream).Write(outputMessage.Payload)
	return err
}

// startLocalConn establishes a new local connection to forward remote server packets to
func (p *BasicPortForwarding) startLocalConn(log log.T) (err error) {
	// When localPortNumber is not specified, set port number to 0 to let net.conn choose an open port at random
	localPortNumber := p.portParameters.LocalPortNumber
	if p.portParameters.LocalPortNumber == "" {
		localPortNumber = "0"
	}

	var listener net.Listener
	if listener, err = p.startLocalListener(log, localPortNumber); err != nil {
		log.Errorf("Unable to open tcp connection to port. %v", err)
		return err
	}

	var tcpConn net.Conn
	if tcpConn, err = acceptConnection(log, listener); err != nil {
		log.Errorf("Failed to accept connection with error. %v", err)
		return err
	}
	log.Infof("Connection accepted for session %s.", p.sessionId)
	fmt.Printf("Connection accepted for session %s.\n", p.sessionId)

	p.listener = &listener
	p.stream = &tcpConn

	return
}

// startLocalListener starts a local listener to given address
func (p *BasicPortForwarding) startLocalListener(log log.T, portNumber string) (listener net.Listener, err error) {
	var displayMessage string
	switch p.portParameters.LocalConnectionType {
	case "unix":
		if listener, err = getNewListener(p.portParameters.LocalConnectionType, p.portParameters.LocalUnixSocket); err != nil {
			return
		}
		displayMessage = fmt.Sprintf("Unix socket %s opened for sessionId %s.", p.portParameters.LocalUnixSocket, p.sessionId)
	default:
		if listener, err = getNewListener("tcp", "localhost:"+portNumber); err != nil {
			return
		}
		// get port number the TCP listener opened
		p.portParameters.LocalPortNumber = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		displayMessage = fmt.Sprintf("Port %s opened for sessionId %s.", p.portParameters.LocalPortNumber, p.sessionId)
	}

	log.Info(displayMessage)
	fmt.Println(displayMessage)
	return
}

// handleControlSignals handles terminate signals
func (p *BasicPortForwarding) handleControlSignals(log log.T) {
	c := make(chan os.Signal)
	signal.Notify(c, sessionutil.ControlSignals...)
	go func() {
		<-c
		fmt.Println("Terminate signal received, exiting.")

		if version.DoesAgentSupportTerminateSessionFlag(log, p.session.DataChannel.GetAgentVersion()) {
			if err := p.session.DataChannel.SendFlag(log, message.TerminateSession); err != nil {
				log.Errorf("Failed to send TerminateSession flag: %v", err)
			}
			fmt.Fprintf(os.Stdout, "\n\nExiting session with sessionId: %s.\n\n", p.sessionId)
			p.Stop()
		} else {
			p.session.TerminateSession(log)
		}
	}()
}

// reconnect closes existing connection, listens to new connection and accept it
func (p *BasicPortForwarding) reconnect(log log.T) (err error) {
	// close existing connection as it is in a state from which data cannot be read
	(*p.stream).Close()

	// wait for new connection on listener and accept it
	var conn net.Conn
	if conn, err = acceptConnection(log, *p.listener); err != nil {
		return log.Errorf("Failed to accept connection with error. %v", err)
	}
	p.stream = &conn

	return
}

/**************** BasicortForwarding ********************************/

/**************** StandardStreamForwarding ********************************/
type StandardStreamForwarding struct {
	port           IPortSession
	inputStream    *os.File
	outputStream   *os.File
	portParameters PortParameters
	session        Session
}

// IsStreamNotSet checks if streams are not set
func (p *StandardStreamForwarding) IsStreamNotSet() (status bool) {
	return p.inputStream == nil || p.outputStream == nil
}

// Stop closes the streams
func (p *StandardStreamForwarding) Stop() {
	p.inputStream.Close()
	p.outputStream.Close()
	os.Exit(0)
}

// InitializeStreams initializes the streams with its file descriptors
func (p *StandardStreamForwarding) InitializeStreams(log log.T) (err error) {
	p.inputStream = os.Stdin
	p.outputStream = os.Stdout
	return
}

// ReadStream reads data from the input stream
func (p *StandardStreamForwarding) ReadStream(log log.T) (err error) {
	msg := make([]byte, config.StreamDataPayloadSize)
	for {
		numBytes, err := p.inputStream.Read(msg)
		if err != nil {
			return p.handleReadError(log, err)
		}

		log.Tracef("Received message of size %d from stdin.", numBytes)
		if err = p.session.DataChannel.SendInputDataMessage(log, message.Output, msg[:numBytes]); err != nil {
			log.Errorf("Failed to send packet: %v", err)
			return err
		}
		// Sleep to process more data
		time.Sleep(time.Millisecond)
	}
}

// WriteStream writes data to output stream
func (p *StandardStreamForwarding) WriteStream(outputMessage message.ClientMessage) error {
	_, err := p.outputStream.Write(outputMessage.Payload)
	return err
}

// handleReadError handles read error
func (p *StandardStreamForwarding) handleReadError(log log.T, err error) error {
	if err == io.EOF {
		log.Infof("Session to instance[%s] on port[%s] was closed.", p.session.TargetId, p.portParameters.PortNumber)
		return nil
	} else {
		log.Errorf("Reading input failed with error: %v", err)
		return err
	}
}

/**************** StandardStreamForwarding ********************************/
