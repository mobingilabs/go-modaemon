package cmd

import (
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/mobingilabs/go-modaemon/api"
	"github.com/mobingilabs/go-modaemon/code"
	"github.com/mobingilabs/go-modaemon/config"
	"github.com/mobingilabs/go-modaemon/container"
	molog "github.com/mobingilabs/go-modaemon/log"
	"github.com/mobingilabs/go-modaemon/login"
	"github.com/mobingilabs/go-modaemon/server_config"
	"github.com/mobingilabs/go-modaemon/util"
	"github.com/urfave/cli"
)

func Start(c *cli.Context) error {
	log.Debug("Step: config.LoadFromFile")

	serverid, err := util.GetServerID(c.GlobalString("provider"))
	if err != nil {
		return err
	}

	conf, err := config.LoadFromFile(c.String("config"))
	if err != nil {
		return err
	}
	log.Debugf("%#v", conf)

	log.Debug("Step: api.NewClient")
	apiClient, err := api.NewClient(conf)
	if err != nil {
		return err
	}
	log.Debugf("%#v", apiClient)

	apiClient.SendInstanceStatus(serverid, "starting")

	stsToken, err := apiClient.GetStsToken()
	if err != nil {
		apiClient.SendInstanceStatus(serverid, "error")
		return err
	}

	apiClient.WriteTempToken(stsToken)

	log.Debug("Step: apiClient.GetServerConfig")
	log.Debugf("Flag: %#v", c.String("serverconfig"))
	s, err := apiClient.GetServerConfig(c.String("serverconfig"))
	if err != nil {
		apiClient.SendInstanceStatus(serverid, "error")
		return err
	}
	log.Debugf("%#v", s)

	for x, y := range s.Users {
		login.EnsureUser(x, y.PublicKey)
	}

	codeDir := ""
	if s.Code != "" {
		code := code.New(s)
		if code.Key != "" {
			log.Debug("Step: code.PrivateRepo")
			err = code.PrivateRepo()
			if err != nil {
				return err
			}
		}

		codeDir, err = code.Get()
		if err != nil {
			return err
		}
	}

	log.Debug("Step: molog.NewDocker")
	ld, err := molog.NewDocker(conf, serverid)
	if err != nil {
		return err
	}
	log.Debugf("%#v", ld)

	log.Debug("Step: ld.StartContainer")
	logContainer, err := ld.StartContainer("mo-awslogs", "", false)
	if err != nil {
		return err
	}
	log.Debugf("%#v", logContainer)

	log.Debug("Step: container.NewDocker")
	d, err := container.NewDocker(conf, s)
	if err != nil {
		apiClient.SendInstanceStatus(serverid, "error")
		return err
	}
	log.Debugf("%#v", d)

	log.Debug("Step: d.StartContainer")
	newContainer, err := d.StartContainer("active", codeDir, true)
	if err != nil {
		apiClient.SendInstanceStatus(serverid, "error")
		return err
	}
	log.Debugf("%#v", newContainer)

	log.Debug("Step: d.MapPort")
	err = d.MapPort(newContainer)
	if err != nil {
		apiClient.SendInstanceStatus(serverid, "error")
		return err
	}

	log.Debug("Step: serverConfig.WriteUpdated")
	if err := serverConfig.WriteUpdated(s); err != nil {
		return err
	}

	var wg sync.WaitGroup
	timer := time.NewTimer(180 * time.Second)
	state := make(chan string)
	done := make(chan bool)
	cancel := make(chan bool)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-cancel:
				log.Error("Container start processing timed out.")
				return
			case s := <-state:
				apiClient.SendInstanceStatus(serverid, s)
				if s == "complete" {
					done <- true
					return
				}
			}
		}
	}()

LOOP:
	for {
		select {
		case <-timer.C:
			cancel <- true
			break LOOP
		case <-done:
			break LOOP
		case state <- util.FetchContainerState():
			time.Sleep(2 * time.Second)
		}
	}

	wg.Wait()
	return nil
}
