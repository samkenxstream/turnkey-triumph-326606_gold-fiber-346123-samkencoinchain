package bc

import (
	"bytes"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"

	"golang.org/x/crypto/sha3"

	"chain/encoding/blockchain"
	"chain/errors"
)

const (
	// CurrentTransactionVersion is the current latest
	// supported transaction version.
	CurrentTransactionVersion = 1

	// InvalidOutputIndex indicates issuance transaction.
	InvalidOutputIndex uint32 = 0xffffffff

	VMVersion = 1
)

const (
	assetDefinitionMaxByteLength = 5000000 // 5 mb
	metadataMaxByteLength        = 500000  // 500 kb
)

// Tx holds a transaction along with its hash.
type Tx struct {
	TxData
	Hash   Hash
	Stored bool // whether this tx is on durable storage
}

func (tx *Tx) UnmarshalText(p []byte) error {
	if err := tx.TxData.UnmarshalText(p); err != nil {
		return err
	}

	tx.Hash = tx.TxData.Hash()
	return nil
}

// NewTx returns a new Tx containing data and its hash.
// If you have already computed the hash, use struct literal
// notation to make a Tx object directly.
func NewTx(data TxData) *Tx {
	return &Tx{
		TxData: data,
		Hash:   data.Hash(),
	}
}

// These flags are part of the wire protocol;
// they must not change.
const (
	SerWitness uint8 = 1 << iota
	SerPrevout
	SerMetadata

	// Bit mask for accepted serialization flags.
	// All other flag bits must be 0.
	SerValid    = 0x7
	serRequired = 0x7 // we support only this combination of flags
)

// TxData encodes a transaction in the blockchain.
// Most users will want to use Tx instead;
// it includes the hash.
type TxData struct {
	SerFlags uint8
	Version  uint32
	Inputs   []*TxInput
	Outputs  []*TxOutput
	MinTime  uint64
	MaxTime  uint64
	Metadata []byte
}

// TxInput encodes a single input in a transaction.
type TxInput struct {
	Previous        Outpoint
	AssetAmount     AssetAmount
	PrevScript      []byte
	SignatureScript []byte
	Metadata        []byte
	AssetDefinition []byte
}

type (
	TxOutput struct {
		AssetVersion uint32
		OutputCommitment
		ReferenceData []byte
	}

	// TODO(bobg): On input, preserve the raw bytes of the output
	// commitment for forward-compatibility.  That will allow us to
	// re-serialize it even if it contains unknown extension fields.
	// (https://github.com/chain-engineering/chain/pull/1093#discussion_r70508484)
	OutputCommitment struct {
		AssetAmount
		VMVersion      uint32
		ControlProgram []byte
	}
)

func NewTxOutput(assetID AssetID, amount uint64, controlProgram, referenceData []byte) *TxOutput {
	return &TxOutput{
		AssetVersion: 1,
		OutputCommitment: OutputCommitment{
			AssetAmount: AssetAmount{
				AssetID: assetID,
				Amount:  amount,
			},
			VMVersion:      1,
			ControlProgram: controlProgram,
		},
		ReferenceData: referenceData,
	}
}

// Outpoint defines a bitcoin data type that is used to track previous
// transaction outputs.
type Outpoint struct {
	Hash  Hash   `json:"hash"`
	Index uint32 `json:"index"`
}

func NewOutpoint(b []byte, index uint32) *Outpoint {
	result := &Outpoint{Index: index}
	copy(result.Hash[:], b)
	return result
}

// HasIssuance returns true if this transaction has an issuance input.
func (tx *TxData) HasIssuance() bool {
	for _, in := range tx.Inputs {
		if in.IsIssuance() {
			return true
		}
	}
	return false
}

// IsIssuance returns true if input's index is 0xffffffff.
func (ti *TxInput) IsIssuance() bool {
	return ti.Previous.Index == InvalidOutputIndex
}

func (tx *TxData) UnmarshalText(p []byte) error {
	b := make([]byte, hex.DecodedLen(len(p)))
	_, err := hex.Decode(b, p)
	if err != nil {
		return err
	}
	return tx.readFrom(bytes.NewReader(b))
}

func (tx *TxData) Scan(val interface{}) error {
	b, ok := val.([]byte)
	if !ok {
		return errors.New("Scan must receive a byte slice")
	}
	return tx.readFrom(bytes.NewReader(b))
}

func (tx *TxData) Value() (driver.Value, error) {
	b := new(bytes.Buffer)
	_, err := tx.WriteTo(b)
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// assumes r has sticky errors
func (tx *TxData) readFrom(r io.Reader) error {
	var serflags [1]byte
	_, err := io.ReadFull(r, serflags[:])
	tx.SerFlags = serflags[0]
	if err == nil && tx.SerFlags != serRequired {
		return fmt.Errorf("unsupported serflags %#x", tx.SerFlags)
	}

	v, _ := blockchain.ReadUvarint(r)
	tx.Version = uint32(v)

	for n, _ := blockchain.ReadUvarint(r); n > 0; n-- {
		ti := new(TxInput)
		err = ti.readFrom(r)
		if err != nil {
			return err
		}
		tx.Inputs = append(tx.Inputs, ti)
	}

	for n, _ := blockchain.ReadUvarint(r); n > 0; n-- {
		to := new(TxOutput)
		err = to.readFrom(r)
		if err != nil {
			return err
		}
		tx.Outputs = append(tx.Outputs, to)
	}

	tx.MinTime, _ = blockchain.ReadUvarint(r)
	tx.MaxTime, _ = blockchain.ReadUvarint(r)
	tx.Metadata, err = blockchain.ReadBytes(r, metadataMaxByteLength)
	return err
}

// assumes r has sticky errors
func (ti *TxInput) readFrom(r io.Reader) (err error) {
	ti.Previous.readFrom(r)
	ti.AssetAmount.readFrom(r)

	ti.PrevScript, err = blockchain.ReadBytes(r, scriptMaxByteLength)
	if err != nil {
		return err
	}
	ti.SignatureScript, err = blockchain.ReadBytes(r, scriptMaxByteLength)
	if err != nil {
		return err
	}
	ti.Metadata, err = blockchain.ReadBytes(r, metadataMaxByteLength)
	if err != nil {
		return err
	}
	ti.AssetDefinition, err = blockchain.ReadBytes(r, assetDefinitionMaxByteLength)
	if err != nil {
		return err
	}
	return nil
}

// assumes r has sticky errors
func (to *TxOutput) readFrom(r io.Reader) (err error) {
	assetVersion, _ := blockchain.ReadUvarint(r)
	to.AssetVersion = uint32(assetVersion)
	err = to.OutputCommitment.readFrom(r, to.AssetVersion)
	if err != nil {
		return err
	}
	to.ReferenceData, err = blockchain.ReadBytes(r, metadataMaxByteLength)
	if err != nil {
		return err
	}
	// read and ignore the (empty) output witness
	_, err = blockchain.ReadBytes(r, commitmentMaxByteLength) // TODO(bobg): What's the right limit here?
	if err != nil {
		return err
	}
	return nil
}

func (oc *OutputCommitment) readFrom(r io.Reader, assetVersion uint32) (err error) {
	b, err := blockchain.ReadBytes(r, commitmentMaxByteLength) // TODO(bobg): Is this the right limit here?
	if err != nil {
		return err
	}
	if assetVersion != 1 {
		return nil
	}
	rb := bytes.NewBuffer(b)
	oc.AssetAmount.readFrom(rb)
	vmVersion, _ := blockchain.ReadUvarint(rb)
	oc.VMVersion = uint32(vmVersion)
	oc.ControlProgram, err = blockchain.ReadBytes(rb, scriptMaxByteLength)
	if err != nil {
		return err
	}
	return nil
}

// assumes r has sticky errors
func (p *Outpoint) readFrom(r io.Reader) {
	io.ReadFull(r, p.Hash[:])
	index, _ := blockchain.ReadUvarint(r)
	p.Index = uint32(index)
}

// Hash computes the hash of the transaction with metadata fields
// replaced by their hashes,
// and stores the result in Hash.
func (tx *TxData) Hash() Hash {
	h := sha3.New256()
	tx.writeTo(h, 0) // error is impossible
	var v Hash
	h.Sum(v[:0])
	return v
}

// WitnessHash is the combined hash of the
// transactions hash and signature data hash.
// It is used to compute the TxRoot of a block.
func (tx *TxData) WitnessHash() Hash {
	var data []byte

	var lenBytes [9]byte
	n := binary.PutUvarint(lenBytes[:], uint64(len(tx.Inputs)))
	data = append(data, lenBytes[:n]...)

	for _, in := range tx.Inputs {
		sigHash := sha3.Sum256(in.SignatureScript)
		data = append(data, sigHash[:]...)
	}

	txHash := tx.Hash()
	dataHash := sha3.Sum256(data)

	return sha3.Sum256(append(txHash[:], dataHash[:]...))
}

// HashForSig generates the hash required for the specified input's
// signature.
func (tx *TxData) HashForSig(idx int, hashType SigHashType) Hash {
	return NewSigHasher(tx).Hash(idx, hashType)
}

type SigHasher struct {
	tx             *TxData
	inputsHash     *Hash
	allOutputsHash *Hash
}

func NewSigHasher(tx *TxData) *SigHasher {
	return &SigHasher{tx: tx}
}

func (s *SigHasher) writeInput(w io.Writer, idx int) {
	s.tx.Inputs[idx].writeTo(w, 0)
}

func (s *SigHasher) writeOutput(w io.Writer, idx int) {
	s.tx.Outputs[idx].writeTo(w, 0)
}

// Use only when hashtype is not "anyone can pay"
func (s *SigHasher) getInputsHash() *Hash {
	if s.inputsHash == nil {
		var hash Hash
		h := sha3.New256()
		w := errors.NewWriter(h)

		blockchain.WriteUvarint(w, uint64(len(s.tx.Inputs)))
		for i := 0; i < len(s.tx.Inputs); i++ {
			s.writeInput(w, i)
		}
		h.Sum(hash[:0])
		s.inputsHash = &hash
	}
	return s.inputsHash
}

func (s *SigHasher) getAllOutputsHash() *Hash {
	if s.allOutputsHash == nil {
		var hash Hash
		h := sha3.New256()
		w := errors.NewWriter(h)
		blockchain.WriteUvarint(w, uint64(len(s.tx.Outputs)))
		for i := 0; i < len(s.tx.Outputs); i++ {
			s.writeOutput(w, i)
		}
		h.Sum(hash[:0])
		s.allOutputsHash = &hash
	}
	return s.allOutputsHash
}

func (s *SigHasher) Hash(idx int, hashType SigHashType) (hash Hash) {
	var inputsHash *Hash
	if hashType&SigHashAnyOneCanPay == 0 {
		inputsHash = s.getInputsHash()
	} else {
		inputsHash = &Hash{}
	}

	var outputCommitment []byte
	if !s.tx.Inputs[idx].IsIssuance() {
		var buf bytes.Buffer
		buf.Write(s.tx.Inputs[idx].AssetAmount.AssetID[:])
		blockchain.WriteUvarint(&buf, s.tx.Inputs[idx].AssetAmount.Amount)
		blockchain.WriteUvarint(&buf, VMVersion)
		blockchain.WriteBytes(&buf, s.tx.Inputs[idx].PrevScript)
		outputCommitment = buf.Bytes()
	}

	var outputsHash *Hash
	switch hashType & sigHashMask {
	case SigHashAll:
		outputsHash = s.getAllOutputsHash()
	case SigHashNone:
		outputsHash = &Hash{}
	case SigHashSingle:
		if idx >= len(s.tx.Outputs) {
			outputsHash = &Hash{}
		} else {
			h := sha3.New256()
			w := errors.NewWriter(h)
			blockchain.WriteUvarint(w, 1)
			s.writeOutput(w, idx)
			var hash Hash
			h.Sum(hash[:0])
			outputsHash = &hash
		}
	}

	h := sha3.New256()
	w := errors.NewWriter(h)
	blockchain.WriteUvarint(w, uint64(s.tx.Version))
	w.Write(inputsHash[:])
	s.writeInput(w, idx)
	blockchain.WriteBytes(w, outputCommitment)
	w.Write(outputsHash[:])
	blockchain.WriteUvarint(w, s.tx.MinTime)
	blockchain.WriteUvarint(w, s.tx.MaxTime)
	writeMetadata(w, s.tx.Metadata, 0)
	w.Write([]byte{byte(hashType)})

	h.Sum(hash[:0])
	return hash
}

// MarshalText satisfies blockchain.TextMarshaller interface
func (tx *TxData) MarshalText() ([]byte, error) {
	var buf bytes.Buffer
	tx.WriteTo(&buf) // error is impossible
	b := make([]byte, hex.EncodedLen(buf.Len()))
	hex.Encode(b, buf.Bytes())
	return b, nil
}

// WriteTo writes tx to w.
func (tx *TxData) WriteTo(w io.Writer) (int64, error) {
	ew := errors.NewWriter(w)
	tx.writeTo(ew, serRequired)
	return ew.Written(), ew.Err()
}

// assumes w has sticky errors
func (tx *TxData) writeTo(w io.Writer, serflags byte) {
	w.Write([]byte{serflags})
	blockchain.WriteUvarint(w, uint64(tx.Version))

	blockchain.WriteUvarint(w, uint64(len(tx.Inputs)))
	for _, ti := range tx.Inputs {
		ti.writeTo(w, serflags)
	}

	blockchain.WriteUvarint(w, uint64(len(tx.Outputs)))
	for _, to := range tx.Outputs {
		to.writeTo(w, serflags)
	}

	blockchain.WriteUvarint(w, tx.MinTime)
	blockchain.WriteUvarint(w, tx.MaxTime)
	writeMetadata(w, tx.Metadata, serflags)
}

// assumes w has sticky errors
func (ti *TxInput) writeTo(w io.Writer, serflags byte) {
	ti.Previous.WriteTo(w)

	if serflags&SerPrevout != 0 {
		ti.AssetAmount.writeTo(w)
		blockchain.WriteBytes(w, ti.PrevScript)
	}

	// Write the signature script or its hash depending on serialization mode.
	// Hashing the hash of the sigscript allows us to prune signatures,
	// redeem scripts and contracts to optimize memory/storage use.
	// Write the metadata or its hash depending on serialization mode.
	if serflags&SerWitness != 0 {
		blockchain.WriteBytes(w, ti.SignatureScript)
	} else {
		blockchain.WriteBytes(w, nil)
	}

	writeMetadata(w, ti.Metadata, serflags)
	writeMetadata(w, ti.AssetDefinition, serflags)
}

// assumes r has sticky errors
func (to *TxOutput) writeTo(w io.Writer, serflags byte) {
	blockchain.WriteUvarint(w, uint64(to.AssetVersion))
	to.OutputCommitment.writeTo(w, to.AssetVersion)
	writeMetadata(w, to.ReferenceData, serflags)
	blockchain.WriteBytes(w, nil) // empty output witness
}

func (oc OutputCommitment) writeTo(w io.Writer, assetVersion uint32) {
	var b bytes.Buffer
	if assetVersion == 1 {
		oc.AssetAmount.writeTo(&b)
		blockchain.WriteUvarint(&b, uint64(oc.VMVersion))
		blockchain.WriteBytes(&b, oc.ControlProgram)
	}
	blockchain.WriteBytes(w, b.Bytes())
}

// String returns the Outpoint in the human-readable form "hash:index".
func (p Outpoint) String() string {
	return p.Hash.String() + ":" + strconv.FormatUint(uint64(p.Index), 10)
}

// WriteTo writes p to w.
// It assumes w has sticky errors.
func (p Outpoint) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(p.Hash[:])
	if err != nil {
		return int64(n), err
	}
	u, err := blockchain.WriteUvarint(w, uint64(p.Index))
	return int64(n + u), err
}

type AssetAmount struct {
	AssetID AssetID `json:"asset_id"`
	Amount  uint64  `json:"amount"`
}

// assumes r has sticky errors
func (a *AssetAmount) readFrom(r io.Reader) {
	io.ReadFull(r, a.AssetID[:])
	a.Amount, _ = blockchain.ReadUvarint(r)
}

// assumes w has sticky errors
func (a AssetAmount) writeTo(w io.Writer) {
	w.Write(a.AssetID[:])
	blockchain.WriteUvarint(w, a.Amount)
}

// assumes w has sticky errors
func writeMetadata(w io.Writer, data []byte, serflags byte) {
	if serflags&SerMetadata != 0 {
		blockchain.WriteBytes(w, data)
	} else {
		h := fastHash(data)
		blockchain.WriteBytes(w, h)
	}
}
