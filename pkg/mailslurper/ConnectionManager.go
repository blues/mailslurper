// Copyright 2013-2018 Adam Presley. All rights reserved
// Use of this source code is governed by the MIT license
// that can be found in the LICENSE file.

package mailslurper

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

/*
A ConnectionManager is responsible for maintaining, closing, and cleaning
client connections. For every connection there is a worker. After an idle
timeout period the manager will forceably close a client connection.
*/
type ConnectionManager struct {
	closeChannel      chan net.Conn
	config            *Configuration
	connectionPool    ConnectionPool
	mu                sync.Mutex
	killServerContext context.Context
	logger            *logrus.Entry
	mailItemChannel   chan *MailItem
	serverPool        *ServerPool
}

/*
NewConnectionManager creates a new struct
*/
func NewConnectionManager(logger *logrus.Entry, config *Configuration, killServerContext context.Context, mailItemChannel chan *MailItem, serverPool *ServerPool) *ConnectionManager {
	closeChannel := make(chan net.Conn, 5)

	result := &ConnectionManager{
		closeChannel:      closeChannel,
		config:            config,
		connectionPool:    NewConnectionPool(),
		killServerContext: killServerContext,
		logger:            logger,
		mailItemChannel:   mailItemChannel,
		serverPool:        serverPool,
	}

	go func() {
		var err error

		for {
			select {
			case connection := <-closeChannel:
				err = result.Close(connection)

				if err != nil && err != io.EOF {
					logger.WithError(err).Errorf("Error closing connection")
				}

				logger.WithField("connection", connection.RemoteAddr().String()).Infof("Connection closed")
				break

			case <-killServerContext.Done():
				return
			}
		}
	}()

	return result
}

/*
Close will close a client connection. The state of the worker
is only used for logging purposes
*/
func (m *ConnectionManager) Close(connection net.Conn) error {
	addr := connection.RemoteAddr().String()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connectionExistsInPool(connection) {
		if !m.isConnectionClosed(connection) {
			m.logger.Infof("Closing connection %s", addr)
			err := m.connectionPool[addr].Connection.Close()
			delete(m.connectionPool, addr)
			return err
		}

		delete(m.connectionPool, addr)
		return nil
	}

	return ConnectionNotExists(addr)
}

func (m *ConnectionManager) connectionExistsInPool(connection net.Conn) bool {
	if _, ok := m.connectionPool[connection.RemoteAddr().String()]; ok {
		return true
	}

	return false
}

func (m *ConnectionManager) isConnectionClosed(connection net.Conn) bool {
	var err error

	temp := []byte{}

	if err = connection.SetReadDeadline(time.Now()); err != nil {
		return true
	}

	if _, err = connection.Read(temp); err == io.EOF {
		return true
	}

	return false
}

/*
New attempts to track a new client connection. The SMTPListener will
use this to track a client connection and its worker
*/
func (m *ConnectionManager) New(connection net.Conn) error {
	var err error
	var worker *SMTPWorker

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connectionExistsInPool(connection) {
		return ConnectionExists(connection.RemoteAddr().String())
	}

	if worker, err = m.serverPool.NextWorker(connection, m.mailItemChannel, m.killServerContext, m.closeChannel); err != nil {
		connection.Close()
		m.logger.WithError(err).Errorf("Error getting next SMTP worker")
		return errors.Wrapf(err, "Error getting work in ConnectionManager")
	}

	m.connectionPool[connection.RemoteAddr().String()] = NewConnectionPoolItem(connection, worker)
	go m.connectionPool[connection.RemoteAddr().String()].Worker.Work()

	return nil
}
