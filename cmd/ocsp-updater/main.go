// Copyright 2014 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"fmt"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cactus/go-statsd-client/statsd"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/streadway/amqp"
	"time"

	// Load both drivers to allow configuring either
	_ "github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/go-sql-driver/mysql"
	_ "github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/mattn/go-sqlite3"

	"database/sql"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/rpc"
	"github.com/letsencrypt/boulder/sa"

	gorp "github.com/letsencrypt/boulder/Godeps/_workspace/src/gopkg.in/gorp.v1"
)

func setupClients(c cmd.Config) (rpc.CertificateAuthorityClient, chan *amqp.Error) {
	ch := cmd.AmqpChannel(c.AMQP.Server)
	closeChan := ch.NotifyClose(make(chan *amqp.Error, 1))

	cac, err := rpc.NewCertificateAuthorityClient(c.AMQP.CA.Client, c.AMQP.CA.Server, ch)
	cmd.FailOnError(err, "Unable to create CA client")

	return cac, closeChan
}

func updateOne(dbMap *gorp.DbMap, oldestLastUpdatedTime time.Time) {
	log := blog.GetAuditLogger()

	tx, err := dbMap.Begin()
	if err != nil {
		tx.Rollback()
		return
	}

	// If there are fewer than this many days left before the currently-signed
	// OCSP response expires, sign a new OCSP response.
	var certificateStatus []core.CertificateStatus
	result, err := tx.Select(&certificateStatus,
		`SELECT * FROM certificateStatus
		 WHERE ocspLastUpdated > ?
		 ORDER BY ocspLastUpdated ASC
		 LIMIT 1`, oldestLastUpdatedTime)

	if err == sql.ErrNoRows {
		log.Info("No OCSP responses needed.")
		return
	} else if err != nil {
		log.Err("Error loading certificate status: " + err.Error())
	} else {
		log.Info(fmt.Sprintf("%+v\n", result))
	}
}

func main() {
	app := cmd.NewAppShell("ocsp-updater")
	app.Action = func(c cmd.Config) {
		// Set up logging
		stats, err := statsd.NewClient(c.Statsd.Server, c.Statsd.Prefix)
		cmd.FailOnError(err, "Couldn't connect to statsd")

		auditlogger, err := blog.Dial(c.Syslog.Network, c.Syslog.Server, c.Syslog.Tag, stats)
		cmd.FailOnError(err, "Could not connect to Syslog")

		// AUDIT[ Error Conditions ] 9cc4d537-8534-4970-8665-4b382abe82f3
		defer auditlogger.AuditPanic()

		blog.SetAuditLogger(auditlogger)

		// Configure DB
		dbMap, err := sa.NewDbMap(c.OCSP.DBDriver, c.OCSP.DBName)
		if err != nil {
			panic(err)
		}
		dbMap.AddTableWithName(core.OcspResponse{}, "ocspResponses").SetKeys(true, "ID")

		cac, closeChan := setupClients(c)

		go func() {
			// sit around and reconnect to AMQP if the channel
			// drops for some reason and repopulate the wfe object
			// with new RA and SA rpc clients.
			for {
				for err := range closeChan {
					auditlogger.Warning(fmt.Sprintf("AMQP Channel closed, will reconnect in 5 seconds: [%s]", err))
					time.Sleep(time.Second * 5)
					cac, closeChan = setupClients(c)
					auditlogger.Warning("Reconnected to AMQP")
				}
			}
		}()

		// Calculate the cut-off timestamp
		dur, err := time.ParseDuration(c.OCSP.MinTimeToExpiry)
		if err != nil {
			panic(err)
		}
		oldestLastUpdatedTime := time.Now().Add(dur)
		auditlogger.Info(fmt.Sprintf("Searching for OCSP reponses older than %s", oldestLastUpdatedTime))

		updateOne(dbMap, oldestLastUpdatedTime)
	}

	app.Run()
}
