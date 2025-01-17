package prover

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/iden3/go-iden3-crypto/keccak256"
	"io"
	"math/big"
	"worldcoin/gnark-mbu/logging"
)

type Parameters struct {
	InputHash    big.Int
	StartIndex   uint32
	PreRoot      big.Int
	PostRoot     big.Int
	IdComms      []big.Int
	MerkleProofs [][]big.Int
}

type Proof struct {
	Proof groth16.Proof
}

type ProvingSystem struct {
	TreeDepth        uint32
	BatchSize        uint32
	ProvingKey       groth16.ProvingKey
	VerifyingKey     groth16.VerifyingKey
	ConstraintSystem constraint.ConstraintSystem
}

func (p *Parameters) ValidateShape(treeDepth uint32, batchSize uint32) error {
	if len(p.IdComms) != int(batchSize) {
		return fmt.Errorf("wrong number of identity commitments: %d", len(p.IdComms))
	}
	if len(p.MerkleProofs) != int(batchSize) {
		return fmt.Errorf("wrong number of merkle proofs: %d", len(p.MerkleProofs))
	}
	for i, proof := range p.MerkleProofs {
		if len(proof) != int(treeDepth) {
			return fmt.Errorf("wrong size of merkle proof for proof %d: %d", i, len(proof))
		}
	}
	return nil
}

func toBytesLE(b []byte) []byte {
	for i := 0; i < len(b)/2; i++ {
		b[i], b[len(b)-i-1] = b[len(b)-i-1], b[i]
	}
	return b
}

// ComputeInputHash computes the input hash to the prover and verifier.
//
// It uses big-endian byte ordering (network ordering) in order to agree with
// Solidity and avoid the need to perform the byte swapping operations on-chain
// where they would increase our gas cost.
func (p *Parameters) ComputeInputHash() error {
	var data []byte
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, p.StartIndex)
	if err != nil {
		return err
	}
	data = append(data, buf.Bytes()...)
	data = append(data, p.PreRoot.Bytes()...)
	data = append(data, p.PostRoot.Bytes()...)
	for _, v := range p.IdComms {
		idBytes := v.Bytes()
		// extend to 32 bytes if necessary, maintaining big-endian ordering
		if len(idBytes) < 32 {
			idBytes = append(make([]byte, 32-len(idBytes)), idBytes...)
		}
		data = append(data, idBytes...)
	}
	hashBytes := keccak256.Hash(data)
	p.InputHash.SetBytes(hashBytes)
	return nil
}

func BuildR1CS(treeDepth uint32, batchSize uint32) (constraint.ConstraintSystem, error) {
	proofs := make([][]frontend.Variable, batchSize)
	for i := 0; i < int(batchSize); i++ {
		proofs[i] = make([]frontend.Variable, treeDepth)
	}
	circuit := MbuCircuit{
		Depth:        int(treeDepth),
		BatchSize:    int(batchSize),
		IdComms:      make([]frontend.Variable, batchSize),
		MerkleProofs: proofs,
	}
	return frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &circuit)
}

func Setup(treeDepth uint32, batchSize uint32) (*ProvingSystem, error) {
	ccs, err := BuildR1CS(treeDepth, batchSize)
	if err != nil {
		return nil, err
	}
	pk, vk, err := groth16.Setup(ccs)
	if err != nil {
		return nil, err
	}
	return &ProvingSystem{treeDepth, batchSize, pk, vk, ccs}, nil
}

func (ps *ProvingSystem) ExportSolidity(writer io.Writer) error {
	return ps.VerifyingKey.ExportSolidity(writer)
}

func (ps *ProvingSystem) Prove(params *Parameters) (*Proof, error) {
	if err := params.ValidateShape(ps.TreeDepth, ps.BatchSize); err != nil {
		return nil, err
	}
	idComms := make([]frontend.Variable, ps.BatchSize)
	for i := 0; i < int(ps.BatchSize); i++ {
		idComms[i] = params.IdComms[i]
	}
	proofs := make([][]frontend.Variable, ps.BatchSize)
	for i := 0; i < int(ps.BatchSize); i++ {
		proofs[i] = make([]frontend.Variable, ps.TreeDepth)
		for j := 0; j < int(ps.TreeDepth); j++ {
			proofs[i][j] = params.MerkleProofs[i][j]
		}
	}
	assignment := MbuCircuit{
		InputHash:    params.InputHash,
		StartIndex:   params.StartIndex,
		PreRoot:      params.PreRoot,
		PostRoot:     params.PostRoot,
		IdComms:      idComms,
		MerkleProofs: proofs,
	}
	witness, err := frontend.NewWitness(&assignment, ecc.BN254.ScalarField())
	if err != nil {
		return nil, err
	}
	logging.Logger().Info().Msg("generating proof")
	proof, err := groth16.Prove(ps.ConstraintSystem, ps.ProvingKey, witness)
	if err != nil {
		return nil, err
	}
	logging.Logger().Info().Msg("proof generated successfully")
	return &Proof{proof}, nil
}

func (ps *ProvingSystem) Verify(inputHash big.Int, proof *Proof) error {
	publicAssignment := MbuCircuit{
		InputHash: inputHash,
		IdComms:   make([]frontend.Variable, ps.BatchSize),
	}
	witness, err := frontend.NewWitness(&publicAssignment, ecc.BN254.ScalarField(), frontend.PublicOnly())
	if err != nil {
		return err
	}
	return groth16.Verify(proof.Proof, ps.VerifyingKey, witness)
}
