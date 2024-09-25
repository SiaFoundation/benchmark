package rhp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"go.sia.tech/core/consensus"
	rhp2 "go.sia.tech/core/rhp/v2"
	rhp3 "go.sia.tech/core/rhp/v3"
	"go.sia.tech/core/types"
)

type (
	// An accountPayment pays for usage using an ephemeral account
	accountPayment struct {
		Account    rhp3.Account
		PrivateKey types.PrivateKey
	}

	// A contractPayment pays for usage using a contract
	contractPayment struct {
		Revision      *rhp2.ContractRevision
		RefundAccount rhp3.Account
		RenterKey     types.PrivateKey
	}

	// A PaymentMethod facilitates payments to the host using either a contract
	// or an ephemeral account
	PaymentMethod interface {
		Pay(amount types.Currency, height uint64) (rhp3.PaymentMethod, bool)
	}

	// A Wallet funds and signs transactions
	Wallet interface {
		Address() types.Address
		FundTransaction(txn *types.Transaction, amount types.Currency, unconfirmed bool) ([]types.Hash256, error)
		SignTransaction(txn *types.Transaction, toSign []types.Hash256, cf types.CoveredFields)
		ReleaseInputs([]types.Transaction, []types.V2Transaction)
	}

	// A ChainManager is used to get the current consensus state
	ChainManager interface {
		TipState() consensus.State
		Tip() types.ChainIndex
	}
)

type (
	// A Session is an RHP3 session with the host
	Session struct {
		hostKey types.PublicKey
		t       *rhp3.Transport

		pt rhp3.HostPriceTable
	}
)

// Pay implements PaymentMethod
func (cp *contractPayment) Pay(amount types.Currency, height uint64) (rhp3.PaymentMethod, bool) {
	req, ok := rhp3.PayByContract(&cp.Revision.Revision, amount, cp.RefundAccount, cp.RenterKey)
	return &req, ok
}

// Pay implements PaymentMethod
func (ap *accountPayment) Pay(amount types.Currency, height uint64) (rhp3.PaymentMethod, bool) {
	expirationHeight := height + 6
	req := rhp3.PayByEphemeralAccount(ap.Account, amount, expirationHeight, ap.PrivateKey)
	return &req, true
}

// RegisterPriceTable registers the price table with the host
func (s *Session) RegisterPriceTable(index types.ChainIndex, payment PaymentMethod) (rhp3.HostPriceTable, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	if err := stream.WriteRequest(rhp3.RPCUpdatePriceTableID, nil); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to write request: %w", err)
	}
	var resp rhp3.RPCUpdatePriceTableResponse
	if err := stream.ReadResponse(&resp, 4096); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to read response: %w", err)
	}

	var pt rhp3.HostPriceTable
	if err := json.Unmarshal(resp.PriceTableJSON, &pt); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to unmarshal price table: %w", err)
	} else if err := s.processPayment(stream, payment, pt.UpdatePriceTableCost, index.Height); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to pay: %w", err)
	}
	var confirmResp rhp3.RPCPriceTableResponse
	if err := stream.ReadResponse(&confirmResp, 4096); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to read response: %w", err)
	}
	s.pt = pt
	return pt, nil
}

// FundAccount funds the account with the given amount
func (s *Session) FundAccount(index types.ChainIndex, account rhp3.Account, payment PaymentMethod, amount types.Currency) (types.Currency, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	if err := stream.WriteRequest(rhp3.RPCFundAccountID, &s.pt.UID); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write request: %w", err)
	}

	req := &rhp3.RPCFundAccountRequest{
		Account: account,
	}
	if err := stream.WriteResponse(req); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write response: %w", err)
	} else if err := s.processPayment(stream, payment, s.pt.FundAccountCost.Add(amount), index.Height); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to pay: %w", err)
	}

	var resp rhp3.RPCFundAccountResponse
	if err := stream.ReadResponse(&resp, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	}
	return resp.Balance, nil
}

// AccountBalance retrieves the balance of the given account
func (s *Session) AccountBalance(index types.ChainIndex, account rhp3.Account, payment PaymentMethod) (types.Currency, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	if err := stream.WriteRequest(rhp3.RPCAccountBalanceID, &s.pt.UID); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write request: %w", err)
	} else if err := s.processPayment(stream, payment, s.pt.AccountBalanceCost, index.Height); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to pay: %w", err)
	}

	req := rhp3.RPCAccountBalanceRequest{
		Account: account,
	}
	if err := stream.WriteResponse(&req); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write response: %w", err)
	}

	var resp rhp3.RPCAccountBalanceResponse
	if err := stream.ReadResponse(&resp, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	}
	return resp.Balance, nil
}

// AppendSector appends a sector to the contract
func (s *Session) AppendSector(index types.ChainIndex, sector *[rhp2.SectorSize]byte, revision *rhp2.ContractRevision, renterKey types.PrivateKey, payment PaymentMethod, budget types.Currency) (types.Currency, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	req := rhp3.RPCExecuteProgramRequest{
		FileContractID: revision.ID(),
		Program: []rhp3.Instruction{
			&rhp3.InstrAppendSector{
				SectorDataOffset: 0,
				ProofRequired:    true,
			},
		},
		ProgramData: sector[:],
	}

	if err := stream.WriteRequest(rhp3.RPCExecuteProgramID, &s.pt.UID); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write request: %w", err)
	} else if err := s.processPayment(stream, payment, s.pt.InitBaseCost.Add(budget), index.Height); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to pay: %w", err)
	} else if err := stream.WriteResponse(&req); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write response: %w", err)
	}
	var cancelToken types.Specifier // unused
	if err := stream.ReadResponse(&cancelToken, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	}

	var resp rhp3.RPCExecuteProgramResponse
	if err := stream.ReadResponse(&resp, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	} else if resp.Error != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to append sector: %w", resp.Error)
	} else if resp.NewSize != revision.Revision.Filesize+rhp2.SectorSize {
		return types.ZeroCurrency, fmt.Errorf("unexpected filesize: %v != %v", resp.NewSize, revision.Revision.Filesize+rhp2.SectorSize)
	}
	//TODO: validate proof
	// revise the contract
	revised := revision.Revision
	revised.RevisionNumber++
	revised.Filesize = resp.NewSize
	revised.FileMerkleRoot = resp.NewMerkleRoot
	revised.ValidProofOutputs = make([]types.SiacoinOutput, len(revision.Revision.ValidProofOutputs))
	revised.MissedProofOutputs = make([]types.SiacoinOutput, len(revision.Revision.MissedProofOutputs))
	for i := range revision.Revision.ValidProofOutputs {
		revised.ValidProofOutputs[i].Address = revision.Revision.ValidProofOutputs[i].Address
		revised.ValidProofOutputs[i].Value = revision.Revision.ValidProofOutputs[i].Value
	}
	for i := range revision.Revision.MissedProofOutputs {
		revised.MissedProofOutputs[i].Address = revision.Revision.MissedProofOutputs[i].Address
		revised.MissedProofOutputs[i].Value = revision.Revision.MissedProofOutputs[i].Value
	}
	// subtract the storage revenue and collateral from the host's missed proof
	// output and add it to the void
	transfer := resp.AdditionalCollateral.Add(resp.FailureRefund)
	revised.MissedProofOutputs[1].Value = revised.MissedProofOutputs[1].Value.Sub(transfer)
	revised.MissedProofOutputs[2].Value = revised.MissedProofOutputs[2].Value.Add(transfer)
	validProofValues := make([]types.Currency, len(revised.ValidProofOutputs))
	for i := range validProofValues {
		validProofValues[i] = revised.ValidProofOutputs[i].Value
	}
	missedProofValues := make([]types.Currency, len(revised.MissedProofOutputs))
	for i := range missedProofValues {
		missedProofValues[i] = revised.MissedProofOutputs[i].Value
	}

	sigHash := hashRevision(revised)
	finalizeReq := rhp3.RPCFinalizeProgramRequest{
		Signature:         renterKey.SignHash(sigHash),
		RevisionNumber:    revised.RevisionNumber,
		ValidProofValues:  validProofValues,
		MissedProofValues: missedProofValues,
	}
	if err := stream.WriteResponse(&finalizeReq); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write response: %w", err)
	}
	var finalizeResp rhp3.RPCFinalizeProgramResponse
	if err := stream.ReadResponse(&finalizeResp, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	}
	revision.Revision = revised
	revision.Signatures = [2]types.TransactionSignature{
		{
			ParentID:       types.Hash256(revised.ParentID),
			PublicKeyIndex: 0,
			CoveredFields: types.CoveredFields{
				FileContractRevisions: []uint64{0},
			},
			Signature: finalizeReq.Signature[:],
		},
		{
			ParentID:       types.Hash256(revised.ParentID),
			PublicKeyIndex: 1,
			CoveredFields: types.CoveredFields{
				FileContractRevisions: []uint64{0},
			},
			Signature: finalizeResp.Signature[:],
		},
	}
	return resp.TotalCost, nil
}

// ReadSector downloads a sector from the host.
func (s *Session) ReadSector(index types.ChainIndex, w io.Writer, root types.Hash256, offset, length uint64, payment PaymentMethod, budget types.Currency) (types.Currency, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	programData := make([]byte, 48)
	binary.LittleEndian.PutUint64(programData[0:8], length)
	binary.LittleEndian.PutUint64(programData[8:16], offset)
	copy(programData[16:], root[:])

	req := rhp3.RPCExecuteProgramRequest{
		Program: []rhp3.Instruction{
			&rhp3.InstrReadSector{
				LengthOffset:     0,
				OffsetOffset:     8,
				MerkleRootOffset: 16,
				ProofRequired:    true,
			},
		},
		ProgramData: programData,
	}

	if err := stream.WriteRequest(rhp3.RPCExecuteProgramID, &s.pt.UID); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write request: %w", err)
	} else if err := s.processPayment(stream, payment, s.pt.InitBaseCost.Add(budget), index.Height); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to pay: %w", err)
	} else if err := stream.WriteResponse(&req); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to write response: %w", err)
	}
	var cancelToken types.Specifier // unused
	if err := stream.ReadResponse(&cancelToken, 4096); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	}

	var resp rhp3.RPCExecuteProgramResponse
	if err := stream.ReadResponse(&resp, 4096+length); err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to read response: %w", err)
	} else if resp.Error != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to append sector: %w", resp.Error)
	} else if len(resp.Output) != int(length) {
		return types.ZeroCurrency, fmt.Errorf("unexpected output length: %v != %v", len(resp.Output), length)
	}
	_, err := io.Copy(w, bytes.NewReader(resp.Output))
	return resp.TotalCost, err
}

// ScanPriceTable retrieves the host's current price table
func (s *Session) ScanPriceTable() (rhp3.HostPriceTable, error) {
	stream := s.t.DialStream()
	defer stream.Close()

	if err := stream.WriteRequest(rhp3.RPCUpdatePriceTableID, nil); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to write request: %w", err)
	}
	var resp rhp3.RPCUpdatePriceTableResponse
	if err := stream.ReadResponse(&resp, 4096); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to read response: %w", err)
	}

	var pt rhp3.HostPriceTable
	if err := json.Unmarshal(resp.PriceTableJSON, &pt); err != nil {
		return rhp3.HostPriceTable{}, fmt.Errorf("failed to unmarshal price table: %w", err)
	}
	return pt, nil
}

// Close closes the underlying transport
func (s *Session) Close() error {
	return s.t.Close()
}

// processPayment processes a payment using the given payment method
func (s *Session) processPayment(stream *rhp3.Stream, method PaymentMethod, amount types.Currency, height uint64) error {
	pm, ok := method.Pay(amount, height)
	if !ok {
		return fmt.Errorf("payment method cannot pay %v", amount)
	}
	switch pm := pm.(type) {
	case *rhp3.PayByEphemeralAccountRequest:
		if err := stream.WriteResponse(&rhp3.PaymentTypeEphemeralAccount); err != nil {
			return fmt.Errorf("failed to write payment request type: %w", err)
		} else if err := stream.WriteResponse(pm); err != nil {
			return fmt.Errorf("failed to write request: %w", err)
		}
	case *rhp3.PayByContractRequest:
		if err := stream.WriteResponse(&rhp3.PaymentTypeContract); err != nil {
			return fmt.Errorf("failed to write payment request type: %w", err)
		} else if err := stream.WriteResponse(pm); err != nil {
			return fmt.Errorf("failed to write request: %w", err)
		}
		var hostSigResp rhp3.PaymentResponse
		if err := stream.ReadResponse(&hostSigResp, 4096); err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}
	}
	return nil
}

// HashRevision returns the hash of rev.
func hashRevision(rev types.FileContractRevision) types.Hash256 {
	h := types.NewHasher()
	rev.EncodeTo(h.E)
	return h.Sum()
}

// ContractPayment creates a new payment method for a contract
func ContractPayment(revision *rhp2.ContractRevision, renterKey types.PrivateKey, refundAccount rhp3.Account) PaymentMethod {
	return &contractPayment{
		Revision:      revision,
		RenterKey:     renterKey,
		RefundAccount: refundAccount,
	}
}

// AccountPayment creates a new payment method for an account
func AccountPayment(account rhp3.Account, privateKey types.PrivateKey) PaymentMethod {
	return &accountPayment{
		Account:    account,
		PrivateKey: privateKey,
	}
}

// NewSession creates a new session with a host
func NewSession(ctx context.Context, hostKey types.PublicKey, hostAddr string) (*Session, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial host: %w", err)
	}
	t, err := rhp3.NewRenterTransport(conn, hostKey)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	return &Session{
		hostKey: hostKey,
		t:       t,
	}, nil
}
