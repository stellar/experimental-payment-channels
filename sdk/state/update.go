package state

import (
	"errors"
	"fmt"

	"github.com/stellar/experimental-payment-channels/sdk/txbuild"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

type Payment struct {
	IterationNumber       int64
	Amount                int64
	CloseSignatures       []xdr.DecoratedSignature
	DeclarationSignatures []xdr.DecoratedSignature
	FromInitiator         bool
}

func (c *Channel) ProposePayment(amount int64) (*Payment, error) {
	if amount <= 0 {
		return nil, errors.New("payment amount must be greater than 0")
	}
	newBalance := int64(0)
	if c.initiator {
		newBalance = c.amount.Amount + amount
	} else {
		newBalance = c.amount.Amount - amount
	}
	txClose, err := txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		InitiatorSigner:            c.initiatorSigner(),
		ResponderSigner:            c.responderSigner(),
		InitiatorEscrow:            c.initiatorEscrowAccount().Address,
		ResponderEscrow:            c.responderEscrowAccount().Address,
		StartSequence:              c.startingSequence,
		IterationNumber:            c.iterationNumber,
		AmountToInitiator:          maxInt64(0, newBalance*-1),
		AmountToResponder:          maxInt64(0, newBalance),
	})
	if err != nil {
		return nil, err
	}
	txClose, err = txClose.Sign(c.networkPassphrase, c.localSigner)
	if err != nil {
		return nil, err
	}
	return &Payment{
		Amount:          amount,
		CloseSignatures: txClose.Signatures(),
		FromInitiator:   c.initiator,
	}, nil
}

func (c *Channel) PaymentTxs(p *Payment) (close, decl *txnbuild.Transaction, err error) {
	var amountFromInitiator, amountFromResponder int64
	if p.FromInitiator {
		amountFromInitiator = p.Amount
	} else {
		amountFromResponder = p.Amount
	}
	newBalance := c.amount.Amount + amountFromInitiator - amountFromResponder
	close, err = txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		InitiatorSigner:            c.initiatorSigner(),
		ResponderSigner:            c.responderSigner(),
		InitiatorEscrow:            c.initiatorEscrowAccount().Address,
		ResponderEscrow:            c.responderEscrowAccount().Address,
		StartSequence:              c.startingSequence,
		IterationNumber:            c.iterationNumber,
		AmountToInitiator:          maxInt64(0, newBalance*-1),
		AmountToResponder:          maxInt64(0, newBalance),
	})
	if err != nil {
		return
	}
	decl, err = txbuild.Declaration(txbuild.DeclarationParams{
		InitiatorEscrow:         c.initiatorEscrowAccount().Address,
		StartSequence:           c.startingSequence,
		IterationNumber:         c.iterationNumber,
		IterationNumberExecuted: 0,
	})
	if err != nil {
		return
	}
	return
}

func (c *Channel) ConfirmPayment(p *Payment) (*Payment, error) {
	var amountFromInitiator, amountFromResponder int64
	if p.FromInitiator {
		amountFromInitiator = p.Amount
	} else {
		amountFromResponder = p.Amount
	}
	newBalance := c.amount.Amount + amountFromInitiator - amountFromResponder

	txClose, txDecl, err := c.PaymentTxs(p)
	if err != nil {
		return nil, err
	}

	// If remote has not signed close, error as is invalid.
	if err := c.verifySigned(txClose, p.CloseSignatures, c.remoteSigner); err != nil {
		return nil, fmt.Errorf("incorrect closing transaction, the one given may have different data: %w", err)
	}

	// If local has not signed close, sign.
	if err := c.verifySigned(txClose, p.CloseSignatures, c.localSigner); err != nil {
		// TODO - differentiate between wrong signature and missing one
		txClose, err = txClose.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return nil, err
		}
	}

	// Local should always sign declaration if have not yet.
	if err := c.verifySigned(txDecl, p.DeclarationSignatures, c.localSigner); err != nil {
		txDecl, err = txDecl.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return nil, err
		}
	}

	p.CloseSignatures = append(p.CloseSignatures, txClose.Signatures()...)
	p.DeclarationSignatures = append(p.DeclarationSignatures, txDecl.Signatures()...)
	c.amount.Amount = newBalance
	return p, nil
}

func maxInt64(x int64, y int64) int64 {
	if x > y {
		return x
	}
	return y
}