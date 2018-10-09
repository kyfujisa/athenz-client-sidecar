package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ghe.corp.yahoo.co.jp/athenz/athenz-tenant-sidecar/config"
	"github.com/kpango/glg"
	"github.com/pkg/errors"
)

type HC interface {
	StartCertUpdater(ctx context.Context)
	GetCertProvider() CertProvider
}

type hc struct {
	certs                 sync.Map
	ip                    string
	hostname              string
	token                 TokenProvider
	athenzURL             string
	athenzPrincipleHeader string
	nextExpire            time.Duration
	lastRefreshed         time.Time
	certExpire            time.Duration
	certExpireMargin      time.Duration
}

type certificate struct {
	AppID string `xml:"appid,attr"`
	Cert  string `xml:",chardata"`
}

type certificates struct {
	Hostname     string        `xml:"hostname,attr"`
	Certificates []certificate `xml:"certificate"`
}

type CertProvider func(string) (string, error)

const (
	zts                     = "zts.athenz.yahoo.co.jp:4443/wsca/v1"
	defaultCertExpireTime   = 30 * time.Minute // maxExpiry for when no certs are returned
	defaultCertExpireMargin = time.Minute      // maxExpiry for when no certs are returned
)

var (
	ErrCertNotFound = errors.New("certification not found")
)

func NewHC(cfg config.HC, prov TokenProvider) (HC, error) {
	exp, err := time.ParseDuration(cfg.CertExpire)
	if err != nil {
		exp = defaultCertExpireTime
	}
	m, err := time.ParseDuration(cfg.CertExpireMargin)
	if err != nil {
		m = defaultCertExpireMargin
	}
	return &hc{
		certs:                 sync.Map{},
		ip:                    config.GetValue(cfg.IP),
		hostname:              config.GetValue(cfg.Hostname),
		token:                 prov,
		athenzURL:             cfg.AthenzURL,
		athenzPrincipleHeader: "Yahoo-Principal-Auth",
		nextExpire:            defaultCertExpireTime,
		lastRefreshed:         time.Now(),
		certExpire:            exp,
		certExpireMargin:      m,
	}, nil
}

func (h *hc) GetCertProvider() CertProvider {
	return h.getCertificate
}

func (h *hc) getCertificate(appID string) (string, error) {
	cert, ok := h.certs.Load(appID)
	if !ok {
		return "", ErrCertNotFound
	}

	return cert.(string), nil
}

func (h *hc) update() error {
	u := fmt.Sprintf("https://%s/containercerts/mh/%s?d=%d&ip=%s", h.athenzURL, h.hostname, time.Hour/time.Second, url.QueryEscape(h.ip))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	token, err := h.token()
	if err != nil {
		return err
	}
	req.Header.Set(h.athenzPrincipleHeader, token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if res.Body != nil {
			io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
		}
	}()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned status code %d", u, res.StatusCode)
	}

	var certs certificates

	err = xml.NewDecoder(res.Body).Decode(&certs)
	if err != nil {
		return err
	}

	maxExpiry := time.Now().Add(365 * 24 * time.Hour)
	earliestExpiry := maxExpiry
	for _, cert := range certs.Certificates {
		exp, err := h.checkExpire(cert.Cert)
		if err != nil {
			continue
		}
		if exp.Before(earliestExpiry) {
			earliestExpiry = exp
		}
		h.certs.Store(cert.AppID, cert.Cert)
	}

	if earliestExpiry != maxExpiry {
		h.nextExpire = earliestExpiry.Sub(time.Now())
	} else {
		h.nextExpire = h.certExpire
	}

	h.lastRefreshed = time.Now()

	return nil
}

func (h *hc) checkExpire(cert string) (time.Time, error) {
	for _, part := range strings.Split(cert, ";") {
		if strings.HasPrefix(part, "t=") {
			v, err := strconv.ParseInt(strings.TrimPrefix(part, "t="), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(v, 0), nil
		}
	}
	return time.Time{}, nil
}

func (h *hc) StartCertUpdater(ctx context.Context) {
	go func() {
		var err error
		tick := time.NewTicker(time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				err = h.update()
				tick.Stop()
				if err != nil {
					glg.Error(err)
					tick = time.NewTicker(time.Second)
				} else {
					tick = time.NewTicker(h.nextExpire - h.certExpireMargin)
				}
			}
		}
	}()
}