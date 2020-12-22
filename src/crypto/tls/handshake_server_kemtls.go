package tls

import (
	"crypto/hmac"
	"crypto/kem"
	"errors"
	"sync/atomic"
)

func (hs *serverHandshakeStateTLS13) handshakeKEMTLS() error {
	c := hs.c

	// derive MS
	if err := hs.readClientKEMCiphertext(); err != nil {
		return err
	}
	if err := hs.readKEMTLSClientFinished(); err != nil {
		return err
	}

	if err := hs.writeKEMTLSServerFinished(); err != nil {
		return err
	}
	if _, err := c.flush(); err != nil {
		return err
	}

	atomic.StoreUint32(&c.handshakeStatus, 1)

	return nil
}

func (hs *serverHandshakeStateTLS13) readClientKEMCiphertext() error {
	c := hs.c

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	kexMsg, ok := msg.(*clientKeyExchangeMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(kexMsg, msg)
	}
	hs.transcript.Write(kexMsg.marshal())

	sk, ok := hs.cert.PrivateKey.(*kem.PrivateKey)
	if !ok {
		c.sendAlert(alertInternalError)
		return errors.New("crypto/tls: private key unexpectedly wrong type")
	}

	ss, err := kem.Decapsulate(sk, kexMsg.ciphertext)
	if err != nil {
		return err
	}

	// derive AHS
	// AHS <- HKDF.Extract(dHS, ss_s)
	ahs := hs.suite.extract(ss, hs.suite.deriveSecret(hs.handshakeSecret, "derived", nil))
	// CAHTS <- HKDF.Expand(AHS, "c ahs traffic", CH..CKC)
	clientSecret := hs.suite.deriveSecret(ahs,
		clientAuthenticatedHandshakeTrafficLabel, hs.transcript)
	c.in.setTrafficSecret(hs.suite, clientSecret)
	// SAHTS <- HKDF.Expand(AHS, "s ahs traffic", CH..CKC)
	serverSecret := hs.suite.deriveSecret(ahs,
		serverAuthenticatedHandshakeTrafficLabel, hs.transcript)
	c.out.setTrafficSecret(hs.suite, serverSecret)

	// compute MS
	// dAHS <- HKDF.Expand(AHS, "derived", nil)
	// MS <- HKDF.Extract(dAHS, 0)
	hs.masterSecret = hs.suite.extract(nil,
		hs.suite.deriveSecret(ahs, "derived", nil))

	return nil
}

func (hs *serverHandshakeStateTLS13) readKEMTLSClientFinished() error {
	c := hs.c

	msg, err := c.readHandshake()
	if err != nil {
		return err
	}

	finished, ok := msg.(*finishedMsg)
	if !ok {
		c.sendAlert(alertUnexpectedMessage)
		return unexpectedMessageError(finished, msg)
	}

	// fk_s <- HKDF.Expand(MS, "s finished", nil)
	expectedMAC := hs.suite.finishedHashKEMTLS(hs.masterSecret, "c", hs.transcript)
	if !hmac.Equal(expectedMAC, finished.verifyData) {
		c.sendAlert(alertDecryptError)
		return errors.New("tls: invalid server finished hash")
	}

	if _, err := hs.transcript.Write(finished.marshal()); err != nil {
		return err
	}

	// CATS <- HKDF.Expand(MS, "c ap traffic", CH..CF)
	clientSecret := hs.suite.deriveSecret(hs.masterSecret, clientApplicationTrafficLabel, hs.transcript)
	c.in.setTrafficSecret(hs.suite, clientSecret)

	err = c.config.writeKeyLog(keyLogLabelClientTraffic, hs.hello.random, clientSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	return nil
}

func (hs *serverHandshakeStateTLS13) writeKEMTLSServerFinished() error {
	c := hs.c

	finished := &finishedMsg{
		verifyData: hs.suite.finishedHashKEMTLS(hs.masterSecret, "s", hs.transcript),
	}

	if _, err := hs.transcript.Write(finished.marshal()); err != nil {
		return err
	}
	if _, err := c.writeRecord(recordTypeHandshake, finished.marshal()); err != nil {
		return err
	}

	// TS <- HKDF.Expand(MS, "s ap traffic", CH..SF)
	hs.trafficSecret = hs.suite.deriveSecret(hs.masterSecret,
		serverApplicationTrafficLabel, hs.transcript)

	c.out.setTrafficSecret(hs.suite, hs.trafficSecret)

	err := c.config.writeKeyLog(keyLogLabelServerTraffic, hs.hello.random, hs.trafficSecret)
	if err != nil {
		c.sendAlert(alertInternalError)
		return err
	}

	if !c.config.SessionTicketsDisabled && c.config.ClientSessionCache != nil {
		c.resumptionSecret = hs.suite.deriveSecret(hs.masterSecret,
			resumptionLabel, hs.transcript)
	}

	c.ekm = hs.suite.exportKeyingMaterial(hs.masterSecret, hs.transcript)

	return nil
}
