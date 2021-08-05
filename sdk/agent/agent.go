// Package agent contains a rudimentary and experimental implementation of an
// agent that coordinates a TCP network connection, initial handshake, and
// channel opens, payments, and closes.
//
// The agent is intended for use in examples only at this point and is not
// intended to be stable or reliable.
package agent

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stellar/experimental-payment-channels/sdk/msg"
	"github.com/stellar/experimental-payment-channels/sdk/state"
	"github.com/stellar/go/amount"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/txnbuild"
)

// BalanceCollector gets the balance of an asset for an account.
type BalanceCollector interface {
	GetBalance(account *keypair.FromAddress, asset state.Asset) (int64, error)
}

// SequenceNumberCollector gets the sequence number for an account.
type SequenceNumberCollector interface {
	GetSequenceNumber(account *keypair.FromAddress) (int64, error)
}

// Submitter submits a transaction to the network.
type Submitter interface {
	SubmitTx(tx *txnbuild.Transaction) error
}

// Agent coordinates a payment channel over a TCP connection.
type Agent struct {
	ObservationPeriodTime      time.Duration
	ObservationPeriodLedgerGap int64
	MaxOpenExpiry              time.Duration
	NetworkPassphrase          string

	SequenceNumberCollector SequenceNumberCollector
	BalanceCollector        BalanceCollector
	Submitter               Submitter

	EscrowAccountKey    *keypair.FromAddress
	EscrowAccountSigner *keypair.Full

	LogWriter io.Writer

	channel *state.Channel

	conn io.ReadWriter

	// closeSignal is not nil if closing or closed, and the chan is closed once
	// the payment channel is closed.
	closeSignal chan struct{}
}

// Channel returns the channel the agent is managing. The channel will be nil if
// the agent has not established a connection or coordinated a channel with
// another participant.
func (a *Agent) Channel() *state.Channel {
	return a.channel
}

// hello sends a hello message to the remote participant over the connection.
func (a *Agent) hello() error {
	enc := msg.NewEncoder(io.MultiWriter(a.conn, a.LogWriter))
	err := enc.Encode(msg.Message{
		Type: msg.TypeHello,
		Hello: &msg.Hello{
			EscrowAccount: *a.EscrowAccountKey,
			Signer:        *a.EscrowAccountSigner.FromAddress(),
		},
	})
	if err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}
	return nil
}

// Open kicks off the open process which will continue after the function
// returns.
func (a *Agent) Open() error {
	if a.conn == nil {
		return fmt.Errorf("not connected")
	}
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}
	open, err := a.channel.ProposeOpen(state.OpenParams{
		ObservationPeriodTime:      a.ObservationPeriodTime,
		ObservationPeriodLedgerGap: a.ObservationPeriodLedgerGap,
		Asset:                      "native",
		ExpiresAt:                  time.Now().Add(a.MaxOpenExpiry),
	})
	if err != nil {
		return fmt.Errorf("proposing open: %w", err)
	}
	enc := msg.NewEncoder(io.MultiWriter(a.conn, a.LogWriter))
	err = enc.Encode(msg.Message{
		Type:        msg.TypeOpenRequest,
		OpenRequest: &open,
	})
	if err != nil {
		return fmt.Errorf("sending open: %w", err)
	}
	return nil
}

// Payment makes a payment of the payment amount to the remote participant using
// the open channel. The process is asynchronous and the function returns
// immediately after the payment is signed and sent to the remote participant.
// The payment is not authorized until the remote participant signs the payment
// and returns the payment.
func (a *Agent) Payment(paymentAmount string) error {
	if a.conn == nil {
		return fmt.Errorf("not connected")
	}
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}
	amountValue, err := amount.ParseInt64(paymentAmount)
	if err != nil {
		return fmt.Errorf("parsing amount %s: %w", paymentAmount, err)
	}
	ca, err := a.channel.ProposePayment(amountValue)
	if err != nil {
		return fmt.Errorf("proposing payment %d: %w", amountValue, err)
	}
	enc := msg.NewEncoder(io.MultiWriter(a.conn, a.LogWriter))
	err = enc.Encode(msg.Message{
		Type:           msg.TypePaymentRequest,
		PaymentRequest: &ca,
	})
	if err != nil {
		return fmt.Errorf("sending payment: %w", err)
	}
	return nil
}

// Close kicks off the close process by synchronously submitting a tx to the
// network to begin the close process, then synchronously coordinating with the
// remote participant to coordinate the close, then synchronously submitting the
// final close tx either after the observation period is waited out.
func (a *Agent) Close() error {
	if a.conn == nil {
		return fmt.Errorf("not connected")
	}
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}
	a.closeSignal = make(chan struct{})
	// Submit declaration tx
	declTx, closeTx, err := a.channel.CloseTxs()
	if err != nil {
		return fmt.Errorf("building declaration tx: %w", err)
	}
	declHash, err := declTx.HashHex(a.NetworkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing close tx: %w", err)
	}
	fmt.Fprintln(a.LogWriter, "submitting declaration", declHash)
	err = a.Submitter.SubmitTx(declTx)
	if err != nil {
		return fmt.Errorf("submitting declaration tx: %w", err)
	}
	// Revising agreement to close early
	fmt.Fprintln(a.LogWriter, "proposing a revised close for immediate submission")
	ca, err := a.channel.ProposeClose()
	if err != nil {
		return fmt.Errorf("proposing the close: %w", err)
	}
	enc := msg.NewEncoder(io.MultiWriter(a.conn, a.LogWriter))
	err = enc.Encode(msg.Message{
		Type:         msg.TypeCloseRequest,
		CloseRequest: &ca,
	})
	if err != nil {
		return fmt.Errorf("error: sending the close proposal: %w\n", err)
	}
	closeHash, err := closeTx.HashHex(a.NetworkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing close tx: %w", err)
	}
	fmt.Fprintln(a.LogWriter, "waiting observation period to submit delayed close tx", closeHash)
	select {
	case <-a.closeSignal:
		fmt.Fprintln(a.LogWriter, "aborting sending delayed close tx", closeHash)
		return nil
	case <-time.After(a.ObservationPeriodTime):
	}
	fmt.Fprintln(a.LogWriter, "submitting delayed close tx", closeHash)
	err = a.Submitter.SubmitTx(closeTx)
	if err != nil {
		return fmt.Errorf("submitting declaration tx: %w", err)
	}
	return nil
}

func (a *Agent) loop() {
	var err error
	recv := msg.NewDecoder(io.TeeReader(a.conn, a.LogWriter))
	send := msg.NewEncoder(io.MultiWriter(a.conn, a.LogWriter))
	for {
		m := msg.Message{}
		err = recv.Decode(&m)
		if err != nil {
			fmt.Fprintf(a.LogWriter, "error reading: %v\n", err)
			break
		}
		err = a.handle(m, send)
		if err != nil {
			fmt.Fprintf(a.LogWriter, "error handling message: %v\n", err)
		}
	}
}

func (a *Agent) handle(m msg.Message, send *msg.Encoder) error {
	fmt.Fprintf(a.LogWriter, "handling %v\n", m.Type)
	handler := handlerMap[m.Type]
	if handler == nil {
		return fmt.Errorf("unrecognized message type %v", m.Type)
	}
	err := handler(a, m, send)
	if err != nil {
		return fmt.Errorf("handling message type %v: %w", m.Type, err)
	}
	return nil
}

var handlerMap = map[msg.Type]func(*Agent, msg.Message, *msg.Encoder) error{
	msg.TypeHello:           (*Agent).handleHello,
	msg.TypeOpenRequest:     (*Agent).handleOpenRequest,
	msg.TypeOpenResponse:    (*Agent).handleOpenResponse,
	msg.TypePaymentRequest:  (*Agent).handlePaymentRequest,
	msg.TypePaymentResponse: (*Agent).handlePaymentResponse,
	msg.TypeCloseRequest:    (*Agent).handleCloseRequest,
	msg.TypeCloseResponse:   (*Agent).handleCloseResponse,
}

func (a *Agent) handleHello(m msg.Message, send *msg.Encoder) error {
	if a.channel != nil {
		return fmt.Errorf("extra hello received when channel already setup")
	}

	h := *m.Hello

	fmt.Fprintf(a.LogWriter, "other's signer: %v\n", h.Signer.Address())
	fmt.Fprintf(a.LogWriter, "other's escrow account: %v\n", h.EscrowAccount.Address())
	escrowAccountSeqNum, err := a.SequenceNumberCollector.GetSequenceNumber(a.EscrowAccountKey)
	if err != nil {
		return err
	}
	otherEscrowAccountSeqNum, err := a.SequenceNumberCollector.GetSequenceNumber(&h.EscrowAccount)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.LogWriter, "escrow account seq: %v\n", escrowAccountSeqNum)
	fmt.Fprintf(a.LogWriter, "other's escrow account seq: %v\n", otherEscrowAccountSeqNum)
	a.channel = state.NewChannel(state.Config{
		NetworkPassphrase: a.NetworkPassphrase,
		MaxOpenExpiry:     a.MaxOpenExpiry,
		Initiator:         a.EscrowAccountKey.Address() > h.EscrowAccount.Address(),
		LocalEscrowAccount: &state.EscrowAccount{
			Address:        a.EscrowAccountKey,
			SequenceNumber: escrowAccountSeqNum,
		},
		RemoteEscrowAccount: &state.EscrowAccount{
			Address:        &h.EscrowAccount,
			SequenceNumber: otherEscrowAccountSeqNum,
		},
		LocalSigner:  a.EscrowAccountSigner,
		RemoteSigner: &h.Signer,
	})
	return nil
}

func (a *Agent) handleOpenRequest(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	openIn := *m.OpenRequest
	open, err := a.channel.ConfirmOpen(openIn)
	if err != nil {
		return fmt.Errorf("confirming open: %w", err)
	}
	fmt.Fprintf(a.LogWriter, "open authorized\n")
	err = send.Encode(msg.Message{
		Type:         msg.TypeOpenResponse,
		OpenResponse: &open,
	})
	if err != nil {
		return fmt.Errorf("encoding open to send back: %w", err)
	}
	return nil
}

func (a *Agent) handleOpenResponse(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	openIn := *m.OpenResponse
	_, err := a.channel.ConfirmOpen(openIn)
	if err != nil {
		return fmt.Errorf("confirming open: %w", err)
	}
	fmt.Fprintf(a.LogWriter, "open authorized\n")
	formationTx, err := a.channel.OpenTx()
	if err != nil {
		return fmt.Errorf("building formation tx: %w", err)
	}
	err = a.Submitter.SubmitTx(formationTx)
	if err != nil {
		return fmt.Errorf("submitting formation tx: %w", err)
	}
	return nil
}

func (a *Agent) handlePaymentRequest(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	paymentIn := *m.PaymentRequest
	payment, err := a.channel.ConfirmPayment(paymentIn)
	if errors.Is(err, state.ErrUnderfunded) {
		fmt.Fprintf(a.LogWriter, "remote is underfunded for this payment based on cached account balances, checking their escrow account...\n")
		var balance int64
		balance, err = a.BalanceCollector.GetBalance(a.channel.RemoteEscrowAccount().Address, a.channel.OpenAgreement().Details.Asset)
		if err != nil {
			return err
		}
		a.channel.UpdateRemoteEscrowAccountBalance(balance)
		payment, err = a.channel.ConfirmPayment(paymentIn)
	}
	if err != nil {
		return fmt.Errorf("confirming payment: %w", err)
	}
	fmt.Fprintf(a.LogWriter, "payment authorized\n")
	err = send.Encode(msg.Message{Type: msg.TypePaymentResponse, PaymentResponse: &payment})
	if err != nil {
		return fmt.Errorf("encoding payment to send back: %w", err)
	}
	return nil
}

func (a *Agent) handlePaymentResponse(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	paymentIn := *m.PaymentResponse
	_, err := a.channel.ConfirmPayment(paymentIn)
	if err != nil {
		return fmt.Errorf("confirming payment: %w", err)
	}
	fmt.Fprintf(a.LogWriter, "payment authorized\n")
	return nil
}

func (a *Agent) handleCloseRequest(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	closeIn := *m.CloseRequest
	close, err := a.channel.ConfirmClose(closeIn)
	if err != nil {
		return fmt.Errorf("confirming close: %v\n", err)
	}
	err = send.Encode(msg.Message{
		Type:          msg.TypeCloseResponse,
		CloseResponse: &close,
	})
	if err != nil {
		return fmt.Errorf("encoding close to send back: %v\n", err)
	}
	fmt.Fprintln(a.LogWriter, "close ready")
	return nil
}

func (a *Agent) handleCloseResponse(m msg.Message, send *msg.Encoder) error {
	if a.channel == nil {
		return fmt.Errorf("no channel")
	}

	closeIn := *m.CloseResponse
	_, err := a.channel.ConfirmClose(closeIn)
	if err != nil {
		return fmt.Errorf("confirming close: %v\n", err)
	}
	fmt.Fprintln(a.LogWriter, "close ready")
	_, closeTx, err := a.channel.CloseTxs()
	if err != nil {
		return fmt.Errorf("building close tx: %w", err)
	}
	hash, err := closeTx.HashHex(a.NetworkPassphrase)
	if err != nil {
		return fmt.Errorf("hashing close tx: %w", err)
	}
	fmt.Fprintln(a.LogWriter, "submitting close", hash)
	err = a.Submitter.SubmitTx(closeTx)
	if err != nil {
		return fmt.Errorf("submitting close tx: %w", err)
	}
	fmt.Fprintln(a.LogWriter, "close successful")
	if a.closeSignal != nil {
		close(a.closeSignal)
	}
	return nil
}