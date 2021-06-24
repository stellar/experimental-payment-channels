package state

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/stellar/experimental-payment-channels/sdk/txbuild"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// The high level steps for creating a channel update should be as follows, where the returned payments
// flow to the next step:
// 1. Sender calls ProposePayment
// 2. Receiver calls ConfirmPayment
// 3. Sender calls ConfirmPayment
// 4. Receiver calls ConfirmPayment

type CloseAgreement struct {
	IterationNumber       int64
	Balance               Amount
	CloseSignatures       []xdr.DecoratedSignature
	DeclarationSignatures []xdr.DecoratedSignature
}

// isEquivalent returns true if all fields for the close agreements are equal not including signatures, else false.
// Two close agreements that are equal may have different signatures depending on who and when this method is called.
func (ca CloseAgreement) isEquivalent(ca2 CloseAgreement) bool {
	return ca.IterationNumber == ca2.IterationNumber && ca.Balance == ca2.Balance
}

func (ca CloseAgreement) isEmpty() bool {
	return ca.IterationNumber == 0 && ca.Amount == (Amount{}) && len(p.CloseSignatures) == 0 && len(p.DeclarationSignatures) == 0
}

func (c *Channel) ProposePayment(amount Amount) (CloseAgreement, error) {
	if amount.Amount <= 0 {
		return CloseAgreement{}, errors.New("payment amount must be greater than 0")
	}
	if amount.Asset != c.latestCloseAgreement.Balance.Asset {
		return CloseAgreement{}, fmt.Errorf("payment asset type is invalid, got: %s want: %s",
			amount.Asset, c.latestCloseAgreement.Balance.Asset)
	}
	newBalance := int64(0)
	if c.initiator {
		newBalance = c.Balance().Amount + amount.Amount
	} else {
		newBalance = c.Balance().Amount - amount.Amount
	}
	txClose, err := txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		InitiatorSigner:            c.initiatorSigner(),
		ResponderSigner:            c.responderSigner(),
		InitiatorEscrow:            c.initiatorEscrowAccount().Address,
		ResponderEscrow:            c.responderEscrowAccount().Address,
		StartSequence:              c.startingSequence,
		IterationNumber:            c.NextIterationNumber(),
		AmountToInitiator:          maxInt64(0, newBalance*-1),
		AmountToResponder:          maxInt64(0, newBalance),
		Asset:                      amount.Asset,
	})
	if err != nil {
		return CloseAgreement{}, err
	}
	txClose, err = txClose.Sign(c.networkPassphrase, c.localSigner)
	if err != nil {
		return CloseAgreement{}, err
	}

	c.latestUnconfirmedCloseAgreement = CloseAgreement{
		IterationNumber: c.NextIterationNumber(),
		Balance:         newBalance,
		CloseSignatures: p.CloseSignatures,
	}
	return c.latestUnconfirmedCloseAgreement, nil
}

func (c *Channel) PaymentTxs(p Payment) (close, decl *txnbuild.Transaction, err error) {
	newBalance := c.newBalance(p)
	close, err = txbuild.Close(txbuild.CloseParams{
		ObservationPeriodTime:      c.observationPeriodTime,
		ObservationPeriodLedgerGap: c.observationPeriodLedgerGap,
		InitiatorSigner:            c.initiatorSigner(),
		ResponderSigner:            c.responderSigner(),
		InitiatorEscrow:            c.initiatorEscrowAccount().Address,
		ResponderEscrow:            c.responderEscrowAccount().Address,
		StartSequence:              c.startingSequence,
		IterationNumber:            c.NextIterationNumber(),
		AmountToInitiator:          maxInt64(0, newBalance.Amount*-1),
		AmountToResponder:          maxInt64(0, newBalance.Amount),
		Asset:                      p.Amount.Asset,
	})
	if err != nil {
		return
	}
	decl, err = txbuild.Declaration(txbuild.DeclarationParams{
		InitiatorEscrow:         c.initiatorEscrowAccount().Address,
		StartSequence:           c.startingSequence,
		IterationNumber:         c.NextIterationNumber(),
		IterationNumberExecuted: 0,
	})
	if err != nil {
		return
	}
	return
}

// ConfirmPayment confirms a payment. The original proposer should only have to call this once, and the
// receiver should call twice. First to sign the payments and store signatures, second to just store the new signatures
// from the other party's confirmation.
func (c *Channel) ConfirmPayment(p Payment) (payment Payment, fullySigned bool, err error) {
	// at the end of this method if a fully signed payment, create a close agreement and clear latest latestUnconfirmedPayment to
	// prepare for the next update. If not fully signed, save latestUnconfirmedPayment, as we are still in the process of confirming.
	// If an error occurred during this process don't save any new state, as something went wrong.
	defer func() {
		if err != nil {
			return
		}
		// TODO - need to not overwrite here?
		ca := CloseAgreement{p.IterationNumber, newBalance, p.CloseSignatures, p.DeclarationSignatures}
		if fullySigned {
			c.latestUnconfirmedCloseAgreement = CloseAgreement{}
			newBalance := c.newBalance(p)
			c.latestCloseAgreement = ca
		} else {
			c.latestUnconfirmedCloseAgreement = ca
		}
	}()

	// validate payment
	if p.IterationNumber != c.NextIterationNumber() {
		return p, fullySigned, fmt.Errorf("invalid payment iteration number, got: %s want: %s",
			strconv.FormatInt(p.IterationNumber, 10), strconv.FormatInt(c.NextIterationNumber(), 10))
	}
	if !c.latestUnconfirmedCloseAgreement.isEmpty() && !c.latestUnconfirmedCloseAgreement.isEquivalent(ca) {
		return p, fullySigned, errors.New("a different unconfirmed payment exists")
	}
	if p.Amount.Asset != c.latestCloseAgreement.Balance.Asset {
		return Payment{}, fullySigned, fmt.Errorf("payment asset type is invalid, got: %s want: %s",
			p.Amount.Asset, c.latestCloseAgreement.Balance.Asset)
	}

	// create payment transactions
	txClose, txDecl, err := c.PaymentTxs(p)
	if err != nil {
		return p, fullySigned, err
	}

	// If remote has not signed close, error as is invalid.
	signed, err := c.verifySigned(txClose, p.CloseSignatures, c.remoteSigner)
	if err != nil {
		return p, fullySigned, fmt.Errorf("verifying close signed by remote: %w", err)
	}
	if !signed {
		return p, fullySigned, fmt.Errorf("verifying close signed by remote: not signed by remote")
	}

	// If local has not signed close, sign.
	signed, err = c.verifySigned(txClose, p.CloseSignatures, c.localSigner)
	if err != nil {
		return p, fullySigned, fmt.Errorf("verifying close signed by local: %w", err)
	}
	if !signed {
		txClose, err = txClose.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return p, fullySigned, fmt.Errorf("signing close with local: %w", err)
		}
		p.CloseSignatures = append(p.CloseSignatures, txClose.Signatures()...)
	}

	// Local should always sign declaration if have not yet.
	signed, err = c.verifySigned(txDecl, p.DeclarationSignatures, c.localSigner)
	if err != nil {
		return p, fullySigned, fmt.Errorf("verifying declaration signed by local: %w", err)
	}
	if !signed {
		txDecl, err = txDecl.Sign(c.networkPassphrase, c.localSigner)
		if err != nil {
			return p, fullySigned, err
		}
		p.DeclarationSignatures = append(p.DeclarationSignatures, txDecl.Signatures()...)
	}

	// If remote has not signed declaration, it is incomplete.
	signed, err = c.verifySigned(txDecl, p.DeclarationSignatures, c.remoteSigner)
	if err != nil {
		return p, fullySigned, fmt.Errorf("verifying declaration signed by remote: %w", err)
	}
	if !signed {
		return p, fullySigned, nil
	}

	// All signatures are present that would be required to submit all
	// transactions in the payment.
	fullySigned = true
	return p, fullySigned, nil
}

func maxInt64(x int64, y int64) int64 {
	if x > y {
		return x
	}
	return y
}
