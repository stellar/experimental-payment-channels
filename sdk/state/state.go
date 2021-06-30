package state

import (
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

type (
	Asset       = txnbuild.Asset
	NativeAsset = txnbuild.NativeAsset
	CreditAsset = txnbuild.CreditAsset
)

type Amount struct {
	Asset  Asset
	Amount int64
}

type EscrowAccount struct {
	Address        *keypair.FromAddress
	SequenceNumber int64
}

type Channel struct {
	networkPassphrase          string

	startingSequence int64
	// TODO - leave execution out for now
	// iterationNumberExecuted int64

	initiator           bool
	localEscrowAccount  *EscrowAccount
	remoteEscrowAccount *EscrowAccount

	localSigner  *keypair.Full
	remoteSigner *keypair.FromAddress

	openAgreement OpenAgreement

	latestAuthorizedCloseAgreement   CloseAgreement
	latestUnauthorizedCloseAgreement CloseAgreement
}

type Config struct {
	NetworkPassphrase          string

	Initiator bool

	LocalEscrowAccount  *EscrowAccount
	RemoteEscrowAccount *EscrowAccount

	LocalSigner  *keypair.Full
	RemoteSigner *keypair.FromAddress
}

func NewChannel(c Config) *Channel {
	channel := &Channel{
		networkPassphrase:          c.NetworkPassphrase,
		initiator:                  c.Initiator,
		localEscrowAccount:         c.LocalEscrowAccount,
		remoteEscrowAccount:        c.RemoteEscrowAccount,
		localSigner:                c.LocalSigner,
		remoteSigner:               c.RemoteSigner,
	}
	return channel
}

func (c *Channel) NextIterationNumber() int64 {
	if !c.latestUnauthorizedCloseAgreement.isEmpty() {
		return c.latestUnauthorizedCloseAgreement.Details.IterationNumber
	}
	return c.latestAuthorizedCloseAgreement.Details.IterationNumber + 1
}

// Balance returns the amount owing from the initiator to the responder, if positive, or
// the amount owing from the responder to the initiator, if negative.
func (c *Channel) Balance() Amount {
	return c.latestAuthorizedCloseAgreement.Details.Balance
}

func (c *Channel) OpenAgreement() OpenAgreement {
	return c.openAgreement
}

func (c *Channel) LatestCloseAgreement() CloseAgreement {
	return c.latestAuthorizedCloseAgreement
}

func (c *Channel) initiatorEscrowAccount() *EscrowAccount {
	if c.initiator {
		return c.localEscrowAccount
	} else {
		return c.remoteEscrowAccount
	}
}

func (c *Channel) responderEscrowAccount() *EscrowAccount {
	if c.initiator {
		return c.remoteEscrowAccount
	} else {
		return c.localEscrowAccount
	}
}

func (c *Channel) initiatorSigner() *keypair.FromAddress {
	if c.initiator {
		return c.localSigner.FromAddress()
	} else {
		return c.remoteSigner
	}
}

func (c *Channel) responderSigner() *keypair.FromAddress {
	if c.initiator {
		return c.remoteSigner
	} else {
		return c.localSigner.FromAddress()
	}
}

func (c *Channel) verifySigned(tx *txnbuild.Transaction, sigs []xdr.DecoratedSignature, signer keypair.KP) (bool, error) {
	hash, err := tx.Hash(c.networkPassphrase)
	if err != nil {
		return false, err
	}
	for _, sig := range sigs {
		if sig.Hint != signer.Hint() {
			continue
		}
		err := signer.Verify(hash[:], sig.Signature)
		if err == nil {
			return true, nil
		}
	}
	return false, nil
}

func appendNewSignatures(oldSignatures []xdr.DecoratedSignature, newSignatures []xdr.DecoratedSignature) []xdr.DecoratedSignature {
	m := make(map[string]bool)
	for _, os := range oldSignatures {
		m[string(os.Signature)] = true
	}

	for _, ns := range newSignatures {
		if !m[string(ns.Signature)] {
			oldSignatures = append(oldSignatures, ns)
		}
	}
	return oldSignatures
}

type TxInfo struct {
	ID        string
	Iteration int
	Type      string // declaration | close
	Seq       int64
}

// helper method
func (t *TxInfo) MyBalance() error {
	return nil
}

type ChannelCheckResponse struct {
	IsContestable   bool
	Asset           Asset
	TriggeredTxInfo TxInfo
	LatestTxInfo    TxInfo
}
