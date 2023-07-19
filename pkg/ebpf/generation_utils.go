// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ebpf

import (
	"fmt"
)

// GenerateRandomAluInstruction provides a random ALU operation with either
// IMM or Reg src that will be applied to a random dst reg.
func GenerateRandomAluInstruction(prog *Program) Instruction {
	op := uint8(prog.GetRNG().RandRange(0x00, 0x0c)) << 4

	var dstReg uint8
	if op == AluMov {
		dstReg = uint8(prog.GetRNG().RandRange(uint64(prog.MinRegister), uint64(prog.MaxRegister)))
	} else {
		dstReg = prog.GetRandomRegister()
	}
	dReg, _ := GetRegisterFromNumber(dstReg)

	var insClass uint8
	if prog.GetRNG().RandRange(0, 1) == 0 {
		insClass = InsClassAlu
	} else {
		insClass = InsClassAlu64
	}

	// Toss another coin to decide if we are going to do an imm alu
	// operation or one that uses a src register.
	var instr Instruction
	if prog.GetRNG().RandRange(0, 1) == 0 {
		instr = generateImmAluInstruction(op, insClass, dReg, prog)
	} else {
		instr = generateRegAluInstruction(op, insClass, dReg, prog)
	}

	return instr
}

// RandomJumpOp generates a random jump operator.
func RandomJumpOp(a *Program) uint8 {
	// https://docs.kernel.org/bpf/instruction-set.html#jump-instructions
	return uint8(a.GetRNG().RandRange(0x00, 0x0d))
}

// IsConditional determines if the operator is not an Exit, Call or JA
// operation.
func IsConditional(op uint8) bool {
	return !(op == JmpExit || op == JmpCALL || op == JmpJA)
}

// GenerateRandomJmpRegInstruction generates a random jump operation where
// the src operand is a random register. The generator functions tell the
// instruction how to generate the true/false branches.
func GenerateRandomJmpRegInstruction(prog *Program, trueBranchGenerator func(prog *Program) Instruction, falseBranchGenerator func(prog *Program) (Instruction, int16)) Instruction {

	var op uint8

	for {
		op = RandomJumpOp(prog)
		// Shift by 4 bits because we need to respect the ebpf encoding:
		// https://docs.kernel.org/bpf/instruction-set.html#id6
		op <<= 4
		if IsConditional(op) {
			break
		}
	}

	dstReg, _ := GetRegisterFromNumber(prog.GetRandomRegister())
	srcReg, _ := GetRegisterFromNumber(prog.GetRandomRegister())
	for srcReg == dstReg {
		srcReg, _ = GetRegisterFromNumber(prog.GetRandomRegister())
	}

	return &RegJMPInstruction{
		BaseInstruction: BaseInstruction{
			Opcode:           op,
			InstructionClass: InsClassJmp,
		},
		DstReg:               dstReg,
		SrcReg:               srcReg,
		trueBranchGenerator:  trueBranchGenerator,
		falseBranchGenerator: falseBranchGenerator,
	}

}

func generateImmAluInstruction(op, insClass uint8, dstReg *Register, prog *Program) Instruction {
	value := int32(prog.GetRNG().RandRange(0, 0xFFFFFFFF))
	switch op {
	case AluRsh, AluLsh, AluArsh:
		var maxShift = int32(64)
		if insClass == InsClassAlu {
			maxShift = 32
		}
		value = value % maxShift
	case AluNeg:
		value = 0
	case AluMov:
		if !prog.IsRegisterInitialized(dstReg.RegisterNumber()) {
			prog.MarkRegisterInitialized(dstReg.RegisterNumber())
		}
	}

	return NewAluImmInstruction(op, insClass, dstReg, value)
}

func generateRegAluInstruction(op, insClass uint8, dstReg *Register, prog *Program) Instruction {
	srcReg, _ := GetRegisterFromNumber(prog.GetRandomRegister())
	// Negation is not supported with Register as src.
	for op == AluNeg {
		op = uint8(prog.GetRNG().RandRange(0x00, 0x0c)) << 4
	}

	if op == AluMov && !prog.IsRegisterInitialized(dstReg.RegisterNumber()) {
		prog.MarkRegisterInitialized(dstReg.RegisterNumber())
	}

	return NewAluRegInstruction(op, insClass, dstReg, srcReg)
}

// InstructionSequence abstracts away the process of creating a sequence of
// ebpf instructions. This should make writing ebpf programs in buzzer
// more readable and easier to achieve.
func InstructionSequence(instructions ...Instruction) (Instruction, error) {
	return instructionSequenceImpl(instructions)
}

// In order to deal with things like nested jumps, the instruction sequence
// feature needs to be a recursive function, hide the actual implementation
// from users using a non exported function.
func instructionSequenceImpl(instructions []Instruction) (Instruction, error) {
	if len(instructions) == 0 {
		// no more instructions to process, break the recursion.
		return nil, nil
	}
	var root, ptr Instruction
	for i := 0; i < len(instructions); i++ {
		instruction := instructions[i]

		if jmpInstr, ok := instruction.(*IMMJMPInstruction); ok {
			if jmpInstr.FalseBranchSize == 0 && jmpInstr.Opcode != JmpExit {
				return nil, fmt.Errorf("Only Exit() and Jmp() can have an offset of 0")
			}
			falseBranchNextInstr, trueBranchNextInstr, err := handleJmpInstruction(instructions[i:], jmpInstr.FalseBranchSize)
			if err != nil {
				return nil, err
			}

			jmpInstr.FalseBranchNextInstr = falseBranchNextInstr
			jmpInstr.TrueBranchNextInstr = trueBranchNextInstr

			if root == nil {
				root = jmpInstr
				ptr = root
			} else {
				ptr.SetNextInstruction(jmpInstr)
			}
			// Break here because handleJmpInstruction should have processed the rest of the ebpf program.
			break
		} else if jmpInstr, ok := instruction.(*RegJMPInstruction); ok {
			if jmpInstr.FalseBranchSize == 0 {
				return nil, fmt.Errorf("JmpReg instruction cannot have jump offset of 0")
			}
			falseBranchNextInstr, trueBranchNextInstr, err := handleJmpInstruction(instructions[i:], jmpInstr.FalseBranchSize)
			if err != nil {
				return nil, err
			}
			jmpInstr.FalseBranchNextInstr = falseBranchNextInstr
			jmpInstr.TrueBranchNextInstr = trueBranchNextInstr

			if root == nil {
				root = jmpInstr
				ptr = root
			} else {
				ptr.SetNextInstruction(jmpInstr)
			} // Break here because handleJmpInstruction should have processed the rest of the ebpf program.
			break
		} else {
			if root == nil {
				root = instruction
				ptr = root
			} else {
				ptr.SetNextInstruction(instruction)
				ptr = instruction
			}
		}
	}
	return root, nil
}

func handleJmpInstruction(instructions []Instruction, offset int16) (Instruction, Instruction, error) {
	if len(instructions) == 0 {
		// TODO: here and below, to improve testing lets define the possible
		// errors in a list somewhere else so we can compare directly that we
		// got the error we expect.
		return nil, nil, fmt.Errorf("handleJmpInstruction invocation should receive at least 1 instruction")
	}
	trueBranchStartIndex := int(offset) + 1
	if trueBranchStartIndex > len(instructions) {
		// TODO: For this error message and others, it would make debugging
		// easier if we could put the offending instruction.
		// For that we would need a way to convert an instruction to a
		// readable string, this is easy to do but let's do it in a follow
		// up patch.
		return nil, nil, fmt.Errorf("Jmp goes out of bounds")
	}

	// instructions[0] should be the jump itself.
	falseBranchInstrs := instructions[1:trueBranchStartIndex]
	trueBranchInstrs := instructions[trueBranchStartIndex:]

	falseBranchNextInstr, err := instructionSequenceImpl(falseBranchInstrs)
	if err != nil {
		return nil, nil, err
	}
	trueBranchNextInstr, err := instructionSequenceImpl(trueBranchInstrs)
	if err != nil {
		return nil, nil, err
	}
	return falseBranchNextInstr, trueBranchNextInstr, nil
}

// Mov64 generates an either MOV_ALU64_IMM or MOV_ALU64_REG operation,
// depending if the src argument is an int or a register, returns nil if
// the supplied value is any other type.
//
// Why golang doesn't have function overloading?!
func Mov64(dstReg *Register, src interface{}) Instruction {
	if srcReg, ok := src.(*Register); ok {
		return NewAluRegInstruction(AluMov, InsClassAlu64, dstReg, srcReg)
	} else if srcImm, ok := src.(int); ok {
		return NewAluImmInstruction(AluMov, InsClassAlu64, dstReg, int32(srcImm))
	} else {
		return nil
	}
}

func Mul64(dstReg *Register, imm int32) Instruction {
	return NewAluImmInstruction(AluMul, InsClassAlu64, dstReg, imm)
}

func Exit() Instruction {
	return &IMMJMPInstruction{BaseInstruction: BaseInstruction{Opcode: JmpExit, InstructionClass: InsClassJmp}, Imm: UnusedField, DstReg: RegR0}
}

func JmpGT(dstReg *Register, imm int32, offset int16) Instruction {
	return &IMMJMPInstruction{BaseInstruction: BaseInstruction{Opcode: JmpJGT, InstructionClass: InsClassJmp}, Imm: UnusedField, DstReg: RegR0, FalseBranchSize: offset}
}

func JmpLT(dstReg *Register, srcReg *Register, offset int16) Instruction {
	return &RegJMPInstruction{BaseInstruction: BaseInstruction{Opcode: JmpJGT, InstructionClass: InsClassJmp}, SrcReg: srcReg, DstReg: RegR0, FalseBranchSize: offset}
}

func Jmp(offset int16) Instruction {
	return &IMMJMPInstruction{
		BaseInstruction: BaseInstruction{Opcode: JmpJA, InstructionClass: InsClassJmp}, Imm: UnusedField, DstReg: RegR0, FalseBranchSize: offset}
}
