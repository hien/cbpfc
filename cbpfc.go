// Package cbpfc implements a cBPF (classic BPF) to eBPF
// (extended BPF, not be confused with cBPF extensions) compiler.
//
// cbpfc can compile cBPF filters to:
//   - C, which can be compiled to eBPF with Clang
//   - eBPF
//
// Both the C and eBPF output are intended to be accepted by the kernel verifier:
//   - All packet loads are guarded with runtime packet length checks
//   - RegA, RegX and M[] are zero initialized as required
//   - Division by zero is guarded by runtime checks
//
// The generated C / eBPF is intended to be embedded into a larger C / eBPF program.
package cbpfc

import (
	"fmt"
	"sort"

	"github.com/pkg/errors"
	"golang.org/x/net/bpf"
)

// Map conditionals to their inverse
var condToInverse = map[bpf.JumpTest]bpf.JumpTest{
	bpf.JumpEqual:          bpf.JumpNotEqual,
	bpf.JumpNotEqual:       bpf.JumpEqual,
	bpf.JumpGreaterThan:    bpf.JumpLessOrEqual,
	bpf.JumpLessThan:       bpf.JumpGreaterOrEqual,
	bpf.JumpGreaterOrEqual: bpf.JumpLessThan,
	bpf.JumpLessOrEqual:    bpf.JumpGreaterThan,
	bpf.JumpBitsSet:        bpf.JumpBitsNotSet,
	bpf.JumpBitsNotSet:     bpf.JumpBitsSet,
}

// pos stores the absolute position of a cBPF instruction
type pos uint

// skips store cBPF jumps, which are relative
type skip uint

// instruction wraps a bpf instruction with it's
// original position
type instruction struct {
	bpf.Instruction
	id pos
}

func (i instruction) String() string {
	return fmt.Sprintf("%d: %v", i.id, i.Instruction)
}

// block contains a linear flow on instructions:
//   - Nothing jumps into the middle of a block
//   - Nothing jumps out of the middle of a block
//
// A block may start or end with any instruction, as any instruction
// can be the target of a jump.
//
// A block also knows what blocks it jumps to. This forms a DAG of blocks.
type block struct {
	insns []instruction

	// Map of absolute instruction positions the last instruction
	// of this block can jump to, to the corresponding block
	jumps map[pos]*block

	// id of the instruction that started this block
	// Unique, but not guaranteed to match insns[0].id after blocks are modified
	id pos

	// True IFF another block jumps to this block as a target
	// A block falling-through to this one does not count
	IsTarget bool
}

// newBlock creates a block with copy of insns
func newBlock(insns []instruction) *block {
	// Copy the insns so blocks can be modified independently
	blockInsns := make([]instruction, len(insns))
	copy(blockInsns, insns)

	return &block{
		insns: blockInsns,
		jumps: make(map[pos]*block),
		id:    insns[0].id,
	}
}

func (b *block) Label() string {
	return fmt.Sprintf("block_%d", b.id)
}

func (b *block) skipToPos(s skip) pos {
	return b.last().id + 1 + pos(s)
}

// Get the target block of a skip
func (b *block) skipToBlock(s skip) *block {
	return b.jumps[b.skipToPos(s)]
}

func (b *block) insert(pos uint, insn instruction) {
	b.insns = append(b.insns[:pos], append([]instruction{insn}, b.insns[pos:]...)...)
}

func (b *block) last() instruction {
	return b.insns[len(b.insns)-1]
}

// packetGuardAbsolute is a "fake" instruction
// that checks the length of the packet for absolute packet loads
type packetGuardAbsolute struct {
	// Length the guard checks. offset + size
	Len uint32
}

// Assemble implements the Instruction Assemble method.
func (p packetGuardAbsolute) Assemble() (bpf.RawInstruction, error) {
	return bpf.RawInstruction{}, errors.Errorf("unsupported")
}

// packetGuardIndirect is a "fake" instruction
// that checks the length of the packet for indirect packet loads
type packetGuardIndirect struct {
	// Length the guard checks. offset + size
	Len uint32
}

// Assemble implements the Instruction Assemble method.
func (p packetGuardIndirect) Assemble() (bpf.RawInstruction, error) {
	return bpf.RawInstruction{}, errors.Errorf("unsupported")
}

// initializeScratch is a "fake" instruction
// that zero initializes a scratch position
type initializeScratch struct {
	// Scratch position that needs to be initialized
	N int
}

// Assemble implements the Instruction Assemble method.
func (i initializeScratch) Assemble() (bpf.RawInstruction, error) {
	return bpf.RawInstruction{}, errors.Errorf("unsupported")
}

// checksXNotZero is a "fake" instruction
// that returns no match if X is 0
type checkXNotZero struct {
}

// Assemble implements the Instruction Assemble method.
func (c checkXNotZero) Assemble() (bpf.RawInstruction, error) {
	return bpf.RawInstruction{}, errors.Errorf("unsupported")
}

// compile compiles a cBPF program to an ordered slice of blocks, with:
// - Registers zero initialized as required
// - Required packet access guards added
// - JumpIf and JumpIfX instructions normalized (see normalizeJumps)
func compile(insns []bpf.Instruction) ([]*block, error) {
	err := validateInstructions(insns)
	if err != nil {
		return nil, err
	}

	instructions := toInstructions(insns)

	normalizeJumps(instructions)

	// Split into blocks
	blocks, err := splitBlocks(instructions)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to compute blocks")
	}

	// Initialize registers
	initializeMemory(blocks)

	// Check we don't divide by zero
	err = addDivideByZeroGuards(blocks)
	if err != nil {
		return nil, err
	}

	// Guard packet loads
	addPacketGuards(blocks)

	return blocks, nil
}

// validateInstructions checks the instructions are valid, and we support them
func validateInstructions(insns []bpf.Instruction) error {
	// Can't do anything meaningful with no instructions
	if len(insns) == 0 {
		return errors.New("can't campile 0 instructions")
	}

	for pc, insn := range insns {
		// Assemble does some input validation
		_, err := insn.Assemble()
		if err != nil {
			return errors.Errorf("can't assemble insnstruction %d: %v", pc, insn)
		}

		switch insn.(type) {
		case bpf.LoadExtension, bpf.RawInstruction:
			return errors.Errorf("unsupported instruction %d: %v", pc, insn)
		}
	}

	return nil
}

func toInstructions(insns []bpf.Instruction) []instruction {
	instructions := make([]instruction, len(insns))

	for pc, insn := range insns {
		instructions[pc] = instruction{
			Instruction: insn,
			id:          pos(pc),
		}
	}

	return instructions
}

// normalizeJumps normalizes conditional jumps to always use skipTrue:
// Jumps that only use skipTrue (skipFalse == 0) are unchanged.
// Jumps that use both skipTrue and skipFalse are unchanged.
// Jumps that only use skipFalse (skipTrue == 0) are inverted to only use skipTrue.
func normalizeJumps(insns []instruction) {
	for pc := range insns {
		switch i := insns[pc].Instruction.(type) {
		case bpf.JumpIf:
			if !shouldInvert(i.SkipTrue, i.SkipFalse) {
				continue
			}

			insns[pc].Instruction = bpf.JumpIf{Cond: condToInverse[i.Cond], Val: i.Val, SkipTrue: i.SkipFalse, SkipFalse: i.SkipTrue}

		case bpf.JumpIfX:
			if !shouldInvert(i.SkipTrue, i.SkipFalse) {
				continue
			}

			insns[pc].Instruction = bpf.JumpIfX{Cond: condToInverse[i.Cond], SkipTrue: i.SkipFalse, SkipFalse: i.SkipTrue}
		}
	}
}

// Check if a conditional jump should be inverted
func shouldInvert(skipTrue, skipFalse uint8) bool {
	return skipTrue == 0 && skipFalse != 0
}

// Traverse instructions until end of first block. Target is absolute start of block.
// Return block-relative jump targets
func visitBlock(insns []instruction, target pos) (*block, []skip) {
	for pc, insn := range insns {
		// Relative jumps from this instruction
		var skips []skip

		switch i := insn.Instruction.(type) {
		case bpf.Jump:
			skips = []skip{skip(i.Skip)}
		case bpf.JumpIf:
			skips = []skip{skip(i.SkipTrue), skip(i.SkipFalse)}
		case bpf.JumpIfX:
			skips = []skip{skip(i.SkipTrue), skip(i.SkipFalse)}

		case bpf.RetA, bpf.RetConstant:
			// No extra targets to visit

		default:
			// Regular instruction, next please!
			continue
		}

		// every insn including this one
		return newBlock(insns[:pc+1]), skips
	}

	// Try to fall through to next block
	return newBlock(insns), []skip{0}
}

// targetBlock is a block that targets (ie jumps) to another block
// used internally by splitBlocks()
type targetBlock struct {
	*block
	// True IFF the block falls through to the other block (skip == 0)
	isFallthrough bool
}

// splitBlocks splits the cBPF into an ordered list of blocks.
//
// The blocks are preserved in the order they are found as this guarantees that
// a block only targets later blocks (cBPF jumps are positive, relative offsets).
// This also mimics the layout of the original cBPF, which is good for debugging.
func splitBlocks(instructions []instruction) ([]*block, error) {
	// Blocks we've visited already
	blocks := []*block{}

	// map of targets to blocks that target them
	// target 0 is for the base case
	targets := map[pos][]targetBlock{
		0: nil,
	}

	// As long as we have un visited targets
	for len(targets) > 0 {
		sortedTargets := sortTargets(targets)

		// Get the first one (not really breadth first, but close enough!)
		target := sortedTargets[0]

		end := len(instructions)
		// If there's a next target, ensure we stop before it
		if len(sortedTargets) > 1 {
			end = int(sortedTargets[1])
		}

		next, nextSkips := visitBlock(instructions[target:end], target)

		// Add skips to our list of things to visit
		for _, s := range nextSkips {
			// Convert relative skip to absolute pos
			t := next.skipToPos(s)

			if t >= pos(len(instructions)) {
				return nil, errors.Errorf("instruction %v flows past last instruction", next.last())
			}

			targets[t] = append(targets[t], targetBlock{next, s == 0})
		}

		jmpBlocks := targets[target]

		// Mark all the blocks that jump to the block we've just visited as doing so
		for _, jmpBlock := range jmpBlocks {
			jmpBlock.jumps[target] = next

			// Not a fallthrough, the block we've just visited is explicitly jumped to
			if !jmpBlock.isFallthrough {
				next.IsTarget = true
			}
		}

		blocks = append(blocks, next)

		// Target is now a block!
		delete(targets, target)
	}

	return blocks, nil
}

// sortTargets sorts the target positions (keys), lowest first
func sortTargets(targets map[pos][]targetBlock) []pos {
	keys := make([]pos, len(targets))

	i := 0
	for k := range targets {
		keys[i] = k
		i++
	}

	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	return keys
}

// addDivideByZeroGuards adds runtime guards / checks to ensure
// the program returns no match when it would otherwise divide by zero.
func addDivideByZeroGuards(blocks []*block) error {
	isDivision := func(op bpf.ALUOp) bool {
		return op == bpf.ALUOpDiv || op == bpf.ALUOpMod
	}

	// Is RegX known to be none 0 at the start of each block
	// We can't divide by RegA, only need to check RegX.
	xNotZero := make(map[*block]bool)

	for _, block := range blocks {
		notZero := xNotZero[block]

		for pc := 0; pc < len(block.insns); pc++ {
			insn := block.insns[pc]

			switch i := insn.Instruction.(type) {
			case bpf.ALUOpConstant:
				if isDivision(i.Op) && i.Val == 0 {
					return errors.Errorf("instruction %v divides by 0", insn)
				}
			case bpf.ALUOpX:
				if isDivision(i.Op) && !notZero {
					block.insert(uint(pc), instruction{Instruction: checkXNotZero{}})
					pc++
					notZero = true
				}
			}

			// check if X clobbered - check is invalidated
			if memWrites(insn.Instruction).regs[bpf.RegX] {
				notZero = false
			}
		}

		// update the status of every block this one jumps to
		for _, target := range block.jumps {
			targetNotZero, ok := xNotZero[target]
			if !ok {
				xNotZero[target] = notZero
				continue
			}

			// x needs to be not zero from every possible path
			xNotZero[target] = targetNotZero && notZero
		}
	}

	return nil
}

// addPacketGuards adds packet guards (absolute and indirect) as required.
//
// Traversing the DAG of blocks (by visiting the blocks a block jumps to),
// we know all packet guards that exist at the start of a given block.
// We can check if the block requires a longer / bigger guard than
// the shortest / least existing guard.
func addPacketGuards(blocks []*block) {
	if len(blocks) == 0 {
		return
	}

	// Guards in effect at the start of each block
	// Can't jump backwards so we only need to traverse blocks once
	absoluteGuards := make(map[*block][]packetGuardAbsolute)
	indirectGuards := make(map[*block][]packetGuardIndirect)

	// first block starts with no guards
	absoluteGuards[blocks[0]] = []packetGuardAbsolute{{Len: 0}}
	indirectGuards[blocks[0]] = []packetGuardIndirect{{Len: 0}}

	for _, block := range blocks {
		absolute := addAbsolutePacketGuard(block, leastAbsoluteGuard(absoluteGuards[block]))
		indirect := addIndirectPacketGuard(block, leastIndirectGuard(indirectGuards[block]))

		for _, target := range block.jumps {
			absoluteGuards[target] = append(absoluteGuards[target], absolute)
			indirectGuards[target] = append(indirectGuards[target], indirect)
		}
	}
}

// leastAbsoluteGuard gets the packet guard with least Len / lowest range
func leastAbsoluteGuard(guards []packetGuardAbsolute) packetGuardAbsolute {
	sort.Slice(guards, func(i, j int) bool {
		return guards[i].Len < guards[j].Len
	})

	return guards[0]
}

// leastIndirectGuard gets the packet guard with least Len / lowest range
func leastIndirectGuard(guards []packetGuardIndirect) packetGuardIndirect {
	sort.Slice(guards, func(i, j int) bool {
		return guards[i].Len < guards[j].Len
	})

	return guards[0]
}

// addAbsolutePacketGuard adds required packet guards to a block knowing the least guard in effect at the start of block.
// The guard in effect at the end of the block is returned (may be nil).
func addAbsolutePacketGuard(block *block, guard packetGuardAbsolute) packetGuardAbsolute {
	var biggestLen uint32

	for _, insn := range block.insns {
		switch i := insn.Instruction.(type) {
		case bpf.LoadAbsolute:
			if a := i.Off + uint32(i.Size); a > biggestLen {
				biggestLen = a
			}
		case bpf.LoadMemShift:
			if a := i.Off + 1; a > biggestLen {
				biggestLen = a
			}
		}
	}

	if biggestLen > guard.Len {
		guard = packetGuardAbsolute{
			Len: biggestLen,
		}
		block.insert(0, instruction{Instruction: guard})
	}

	return guard
}

// addIndirectPacketGuard adds required packet guards to a block knowing the least guard in effect at the start of block.
// The guard in effect at the end of the block is returned (may be nil).
func addIndirectPacketGuard(block *block, guard packetGuardIndirect) packetGuardIndirect {
	var biggestLen, start uint32

	for pc := 0; pc < len(block.insns); pc++ {
		insn := block.insns[pc]

		switch i := insn.Instruction.(type) {
		case bpf.LoadIndirect:
			if a := i.Off + uint32(i.Size); a > biggestLen {
				biggestLen = a
			}
		}

		// Check if we clobbered x - this invalidates the guard
		clobbered := memWrites(insn.Instruction).regs[bpf.RegX]

		// End of block or x clobbered -> create guard for previous instructions
		if pc == len(block.insns)-1 || clobbered {
			if biggestLen > guard.Len {
				guard = packetGuardIndirect{
					Len: biggestLen,
				}
				block.insert(uint(start), instruction{Instruction: guard})
				pc++ // Skip the instruction we've just added
			}
		}

		if clobbered {
			// New pseudo block starts here
			start = uint32(pc) + 1
			guard = packetGuardIndirect{Len: 0}
			biggestLen = 0
		}
	}

	return guard
}

// memStatus represents a context defined status of registers & scratch
type memStatus struct {
	// indexed by bpf.Register
	regs    [2]bool
	scratch [16]bool
}

// merge merges this status with the other by applying policy to regs and scratch
func (r memStatus) merge(other memStatus, policy func(this, other bool) bool) memStatus {
	newStatus := memStatus{}

	for i := range newStatus.regs {
		newStatus.regs[i] = policy(r.regs[i], other.regs[i])
	}

	for i := range newStatus.scratch {
		newStatus.scratch[i] = policy(r.scratch[i], other.scratch[i])
	}

	return newStatus
}

// and merges this status with the other by logical AND
func (r memStatus) and(other memStatus) memStatus {
	return r.merge(other, func(this, other bool) bool {
		return this && other
	})
}

// and merges this status with the other by logical OR
func (r memStatus) or(other memStatus) memStatus {
	return r.merge(other, func(this, other bool) bool {
		return this || other
	})
}

// initializeMemory zero initializes all the memory (regs & scratch) that the BPF program reads from before writing to.
func initializeMemory(blocks []*block) {
	// memory initialized at the start of each block
	statuses := make(map[*block]memStatus)

	// uninitialized memory used so far
	uninitialized := memStatus{}

	for _, block := range blocks {
		status := statuses[block]

		for _, insn := range block.insns {
			uninitialized = uninitialized.or(memUninitializedReads(insn.Instruction, status))
			status = status.or(memWrites(insn.Instruction))
		}

		// update the status of every block this one jumps to
		for _, target := range block.jumps {
			targetStatus, ok := statuses[target]
			if !ok {
				statuses[target] = status
				continue
			}

			// memory needs to be initialized from every possible path
			statuses[target] = targetStatus.and(status)
		}
	}

	for reg, uninit := range uninitialized.regs {
		if !uninit {
			continue
		}

		blocks[0].insert(0, instruction{
			Instruction: bpf.LoadConstant{
				Dst: bpf.Register(reg),
				Val: 0,
			},
		})
	}

	for scratch, uninit := range uninitialized.scratch {
		if !uninit {
			continue
		}

		blocks[0].insert(0, instruction{
			Instruction: initializeScratch{
				N: scratch,
			},
		})
	}
}

// memUninitializedReads returns the memory read by insn that has not yet been initialized according to initialized.
func memUninitializedReads(insn bpf.Instruction, initialized memStatus) memStatus {
	return memReads(insn).merge(initialized, func(read, init bool) bool {
		return read && !init
	})
}

// memReads returns the memory read by insn
func memReads(insn bpf.Instruction) memStatus {
	read := memStatus{}

	switch i := insn.(type) {
	case bpf.ALUOpConstant:
		read.regs[bpf.RegA] = true
	case bpf.ALUOpX:
		read.regs[bpf.RegA] = true
		read.regs[bpf.RegX] = true

	case bpf.JumpIf:
		read.regs[bpf.RegA] = true
	case bpf.JumpIfX:
		read.regs[bpf.RegA] = true
		read.regs[bpf.RegX] = true

	case bpf.LoadIndirect:
		read.regs[bpf.RegX] = true
	case bpf.LoadScratch:
		read.scratch[i.N] = true

	case bpf.NegateA:
		read.regs[bpf.RegA] = true

	case bpf.RetA:
		read.regs[bpf.RegA] = true

	case bpf.StoreScratch:
		read.regs[i.Src] = true

	case bpf.TAX:
		read.regs[bpf.RegA] = true
	case bpf.TXA:
		read.regs[bpf.RegX] = true
	}

	return read
}

// memWrites returns the memory written by insn
func memWrites(insn bpf.Instruction) memStatus {
	write := memStatus{}

	switch i := insn.(type) {
	case bpf.ALUOpConstant:
		write.regs[bpf.RegA] = true
	case bpf.ALUOpX:
		write.regs[bpf.RegA] = true

	case bpf.LoadAbsolute:
		write.regs[bpf.RegA] = true
	case bpf.LoadConstant:
		write.regs[i.Dst] = true
	case bpf.LoadIndirect:
		write.regs[bpf.RegA] = true
	case bpf.LoadMemShift:
		write.regs[bpf.RegX] = true
	case bpf.LoadScratch:
		write.regs[i.Dst] = true

	case bpf.NegateA:
		write.regs[bpf.RegA] = true

	case bpf.StoreScratch:
		write.scratch[i.N] = true

	case bpf.TAX:
		write.regs[bpf.RegX] = true
	case bpf.TXA:
		write.regs[bpf.RegA] = true
	}

	return write
}
