package main

import "fmt"

import "runtime"
import "runtime/debug"
import "sync/atomic"
import "sync"
import "time"
import "unsafe"

import "ahci"
import "apic"
import "bnet"
import "caller"
import "defs"
import "inet"
import "fd"
import "fdops"
import "fs"

import "ixgbe"
import "mem"
import "pci"
import "proc"
import "res"
import "stat"
import "stats"
import "tinfo"
import "ustr"
import "vm"

const (
	IRQ_LAST = defs.INT_MSI7
)

// trapstub() cannot do anything that may have side-effects on the runtime
// (like allocate, fmt.Print, or use panic) since trapstub() runs in interrupt
// context and thus may run concurrently with code manipulating the same state.
// since trapstub() runs on the per-cpu interrupt stack, it must be nosplit.
//go:nosplit
func trapstub(tf *[defs.TFSIZE]uintptr) {

	trapno := tf[defs.TF_TRAP]

	// only IRQs come through here now
	if trapno <= defs.TIMER || trapno > IRQ_LAST {
		runtime.Pnum(0x1001)
		for {
		}
	}

	stats.Nirqs[trapno]++
	stats.Irqs++
	switch trapno {
	case defs.INT_KBD, defs.INT_COM1:
		runtime.IRQwake(uint(trapno))
		// we need to mask the interrupt on the IOAPIC since my
		// hardware's LAPIC automatically send EOIs to IOAPICS when the
		// LAPIC receives an EOI and does not support disabling these
		// automatic EOI broadcasts (newer LAPICs do). its probably
		// better to disable automatic EOI broadcast and send EOIs to
		// the IOAPICs in the driver (as the code used to when using
		// 8259s).

		// masking the IRQ on the IO APIC must happen before writing
		// EOI to the LAPIC (otherwise the CPU will probably receive
		// another interrupt from the same IRQ). the LAPIC EOI happens
		// in the runtime...
		irqno := int(trapno - defs.IRQ_BASE)
		apic.Apic.Irq_mask(irqno)
	case defs.INT_MSI0, defs.INT_MSI1, defs.INT_MSI2, defs.INT_MSI3,
		defs.INT_MSI4, defs.INT_MSI5, defs.INT_MSI6, defs.INT_MSI7:
		// MSI dispatch doesn't use the IO APIC, thus no need for
		// irq_mask
		runtime.IRQwake(uint(trapno))
	default:
		// unexpected IRQ
		runtime.Pnum(int(trapno))
		runtime.Pnum(int(tf[defs.TF_RIP]))
		runtime.Pnum(0xbadbabe)
		for {
		}
	}
}

var ide_int_done = make(chan bool)

func trap_disk(intn uint) {
	for {
		runtime.IRQsched(intn)

		// is this a disk int?
		if !pci.Disk.Intr() {
			fmt.Printf("spurious disk int\n")
			return
		}
		ide_int_done <- true
	}
}

func trap_cons(intn uint, ch chan bool) {
	for {
		runtime.IRQsched(intn)
		ch <- true
	}
}

func tfdump(tf *[defs.TFSIZE]int) {
	fmt.Printf("RIP: %#x\n", tf[defs.TF_RIP])
	fmt.Printf("RAX: %#x\n", tf[defs.TF_RAX])
	fmt.Printf("RDI: %#x\n", tf[defs.TF_RDI])
	fmt.Printf("RSI: %#x\n", tf[defs.TF_RSI])
	fmt.Printf("RBX: %#x\n", tf[defs.TF_RBX])
	fmt.Printf("RCX: %#x\n", tf[defs.TF_RCX])
	fmt.Printf("RDX: %#x\n", tf[defs.TF_RDX])
	fmt.Printf("RSP: %#x\n", tf[defs.TF_RSP])
}

type dev_t struct {
	major int
	minor int
}

var dummyfops = &fs.Devfops_t{Maj: defs.D_CONSOLE, Min: 0}

// special fds
var fd_stdin = fd.Fd_t{Fops: dummyfops, Perms: fd.FD_READ}
var fd_stdout = fd.Fd_t{Fops: dummyfops, Perms: fd.FD_WRITE}
var fd_stderr = fd.Fd_t{Fops: dummyfops, Perms: fd.FD_WRITE}

// a userio_i type that copies nothing. useful as an argument to {send,recv}msg
// when no from/to address or ancillary data is requested.
type _nilbuf_t struct {
}

func (nb *_nilbuf_t) Uiowrite(src []uint8) (int, defs.Err_t) {
	return 0, 0
}

func (nb *_nilbuf_t) Uioread(dst []uint8) (int, defs.Err_t) {
	return 0, 0
}

func (nb *_nilbuf_t) Remain() int {
	return 0
}

func (nb *_nilbuf_t) Totalsz() int {
	return 0
}

var zeroubuf = &_nilbuf_t{}

type passfd_t struct {
	cb   []*fd.Fd_t
	inum uint
	cnum uint
}

func (pf *passfd_t) add(nfd *fd.Fd_t) bool {
	if pf.cb == nil {
		pf.cb = make([]*fd.Fd_t, 10)
	}
	l := uint(len(pf.cb))
	if pf.inum-pf.cnum == l {
		return false
	}
	pf.cb[pf.inum%l] = nfd
	pf.inum++
	return true
}

func (pf *passfd_t) take() (*fd.Fd_t, bool) {
	l := uint(len(pf.cb))
	if pf.inum == pf.cnum {
		return nil, false
	}
	ret := pf.cb[pf.cnum%l]
	pf.cnum++
	return ret, true
}

func (pf *passfd_t) closeall() {
	for {
		fd, ok := pf.take()
		if !ok {
			break
		}
		fd.Fops.Close()
	}
}

func cpus_stack_init(apcnt int, stackstart uintptr) {
	for i := 0; i < apcnt; i++ {
		// allocate/map interrupt stack
		vm.Kmalloc(stackstart, vm.PTE_W)
		stackstart += vm.PGSIZEW
		vm.Assert_no_va_map(mem.Kpmap(), stackstart)
		stackstart += vm.PGSIZEW
		// allocate/map NMI stack
		vm.Kmalloc(stackstart, vm.PTE_W)
		stackstart += vm.PGSIZEW
		vm.Assert_no_va_map(mem.Kpmap(), stackstart)
		stackstart += vm.PGSIZEW
	}
}

func cpus_start(ncpu, aplim int) {
	if aplim+1 >= 1<<8 {
		fmt.Printf("Logical CPU IDs overflow 8 bits for PMC profiling\n")
	}
	runtime.GOMAXPROCS(1 + aplim)
	apcnt := ncpu - 1

	fmt.Printf("found %v CPUs\n", ncpu)

	if apcnt == 0 {
		fmt.Printf("uniprocessor\n")
		return
	}

	// AP code must be between 0-1MB because the APs are in real mode. load
	// code to 0x8000 (overwriting bootloader)
	mpaddr := mem.Pa_t(0x8000)
	mpcode := allbins["src/kernel/mpentry.bin"].data
	c := mem.Pa_t(0)
	mpl := mem.Pa_t(len(mpcode))
	for c < mpl {
		mppg := physmem.Dmap8(mpaddr + c)
		did := copy(mppg, mpcode)
		mpcode = mpcode[did:]
		c += mem.Pa_t(did)
	}

	// skip mucking with CMOS reset code/warm reset vector (as per the the
	// "universal startup algoirthm") and instead use the STARTUP IPI which
	// is supported by lapics of version >= 1.x. (the runtime panicks if a
	// lapic whose version is < 1.x is found, thus assume their absence).
	// however, only one STARTUP IPI is accepted after a CPUs RESET or INIT
	// pin is asserted, thus we need to send an INIT IPI assert first (it
	// appears someone already used a STARTUP IPI; probably the BIOS).

	lapaddr := 0xfee00000
	pte := vm.Pmap_lookup(mem.Kpmap(), lapaddr)
	if pte == nil || *pte&mem.PTE_P == 0 || *pte&mem.PTE_PCD == 0 {
		panic("lapaddr unmapped")
	}
	lap := (*[mem.PGSIZE / 4]uint32)(unsafe.Pointer(uintptr(lapaddr)))
	icrh := 0x310 / 4
	icrl := 0x300 / 4

	ipilow := func(ds int, t int, l int, deliv int, vec int) uint32 {
		return uint32(ds<<18 | t<<15 | l<<14 |
			deliv<<8 | vec)
	}

	icrw := func(hi uint32, low uint32) {
		// use sync to guarantee order
		atomic.StoreUint32(&lap[icrh], hi)
		atomic.StoreUint32(&lap[icrl], low)
		ipisent := uint32(1 << 12)
		for atomic.LoadUint32(&lap[icrl])&ipisent != 0 {
		}
	}

	// destination shorthands:
	// 1: self
	// 2: all
	// 3: all but me

	initipi := func(assert bool) {
		vec := 0
		delivmode := 0x5
		level := 1
		trig := 0
		dshort := 3
		if !assert {
			trig = 1
			level = 0
			dshort = 2
		}
		hi := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	startupipi := func() {
		vec := int(mpaddr >> 12)
		delivmode := 0x6
		level := 0x1
		trig := 0x0
		dshort := 0x3

		hi := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	// pass arguments to the ap startup code via secret storage (the old
	// boot loader page at 0x7c00)

	// secret storage layout
	// 0 - e820map
	// 1 - pmap
	// 2 - firstfree
	// 3 - ap entry
	// 4 - gdt
	// 5 - gdt
	// 6 - idt
	// 7 - idt
	// 8 - ap count
	// 9 - stack start
	// 10- proceed

	ss := (*[11]uintptr)(unsafe.Pointer(uintptr(0x7c00)))
	sap_entry := 3
	sgdt := 4
	sidt := 6
	sapcnt := 8
	sstacks := 9
	sproceed := 10
	ss[sap_entry] = runtime.FuncPC(ap_entry)
	// sgdt and sidt save 10 bytes
	runtime.Sgdt(&ss[sgdt])
	runtime.Sidt(&ss[sidt])
	atomic.StoreUintptr(&ss[sapcnt], 0)
	// for BSP:
	// 	int stack	[0xa100000000, 0xa100001000)
	// 	guard page	[0xa100001000, 0xa100002000)
	// 	NMI stack	[0xa100002000, 0xa100003000)
	// 	guard page	[0xa100003000, 0xa100004000)
	// for each AP:
	// 	int stack	BSP's + apnum*4*mem.PGSIZE + 0*mem.PGSIZE
	// 	NMI stack	BSP's + apnum*4*mem.PGSIZE + 2*mem.PGSIZE
	stackstart := uintptr(0xa100004000)
	// each ap grabs a unique stack
	atomic.StoreUintptr(&ss[sstacks], stackstart)
	atomic.StoreUintptr(&ss[sproceed], 0)

	dummy := int64(0)
	atomic.CompareAndSwapInt64(&dummy, 0, 10)

	initipi(true)
	// not necessary since we assume lapic version >= 1.x (ie not 82489DX)
	//initipi(false)
	time.Sleep(10 * time.Millisecond)

	startupipi()
	time.Sleep(10 * time.Millisecond)
	startupipi()

	// wait a while for hopefully all APs to join.
	time.Sleep(500 * time.Millisecond)
	apcnt = int(atomic.LoadUintptr(&ss[sapcnt]))
	if apcnt > aplim {
		apcnt = aplim
	}
	set_cpucount(apcnt + 1)

	// actually map the stacks for the CPUs that joined
	cpus_stack_init(apcnt, stackstart)

	// tell the cpus to carry on
	atomic.StoreUintptr(&ss[sproceed], uintptr(apcnt))

	fmt.Printf("done! %v APs found (%v joined)\n", ss[sapcnt], apcnt)
}

// myid is a logical id, not lapic id
//go:nosplit
func ap_entry(myid uint) {

	// myid starts from 1
	runtime.Ap_setup(myid)

	// ints are still cleared. wait for timer int to enter scheduler
	fl := runtime.Pushcli()
	fl |= defs.TF_FL_IF
	runtime.Popcli(fl)
	for {
	}
}

func set_cpucount(n int) {
	vm.Numcpus = n
	runtime.Setncpu(int32(n))
}

func irq_unmask(irq int) {
	apic.Apic.Irq_unmask(irq)
}

func irq_eoi(irq int) {
	//apic.eoi(irq)
	apic.Apic.Irq_unmask(irq)
}

func kbd_init() {
	km := make(map[int]byte)
	NO := byte(0)
	tm := []byte{
		// ty xv6
		NO, 0x1B, '1', '2', '3', '4', '5', '6', // 0x00
		'7', '8', '9', '0', '-', '=', '\b', '\t',
		'q', 'w', 'e', 'r', 't', 'y', 'u', 'i', // 0x10
		'o', 'p', '[', ']', '\n', NO, 'a', 's',
		'd', 'f', 'g', 'h', 'j', 'k', 'l', ';', // 0x20
		'\'', '`', NO, '\\', 'z', 'x', 'c', 'v',
		'b', 'n', 'm', ',', '.', '/', NO, '*', // 0x30
		NO, ' ', NO, NO, NO, NO, NO, NO,
		NO, NO, NO, NO, NO, NO, NO, '7', // 0x40
		'8', '9', '-', '4', '5', '6', '+', '1',
		'2', '3', '0', '.', NO, NO, NO, NO, // 0x50
	}

	for i, c := range tm {
		if c != NO {
			km[i] = c
		}
	}
	cons.kbd_int = make(chan bool)
	cons.com_int = make(chan bool)
	cons.reader = make(chan []byte)
	cons.reqc = make(chan int)
	cons.pollc = make(chan fdops.Pollmsg_t)
	cons.pollret = make(chan fdops.Ready_t)
	go kbd_daemon(&cons, km)
	irq_unmask(defs.IRQ_KBD)
	irq_unmask(defs.IRQ_COM1)

	// make sure kbd int and com1 int are clear
	for _kready() {
		runtime.Inb(0x60)
	}
	for _comready() {
		runtime.Inb(0x3f8)
	}

	go trap_cons(defs.INT_KBD, cons.kbd_int)
	go trap_cons(defs.INT_COM1, cons.com_int)
}

type cons_t struct {
	kbd_int chan bool
	com_int chan bool
	reader  chan []byte
	reqc    chan int
	pollc   chan fdops.Pollmsg_t
	pollret chan fdops.Ready_t
}

var cons = cons_t{}

func _comready() bool {
	com1ctl := uint16(0x3f8 + 5)
	b := runtime.Inb(com1ctl)
	if b&0x01 == 0 {
		return false
	}
	return true
}

func _kready() bool {
	ibf := uint(1 << 0)
	st := runtime.Inb(0x64)
	if st&ibf == 0 {
		//panic("no kbd data?")
		return false
	}
	return true
}

func loping() {
	fmt.Printf("POING\n")
	sip, dip, err := bnet.Routetbl.Lookup(inet.Ip4_t(0x7f000001))
	if err != 0 {
		panic("error")
	}
	dmac, err := bnet.Arp_resolve(sip, dip)
	if err != 0 {
		panic("error")
	}
	nic, ok := bnet.Nic_lookup(sip)
	if !ok {
		panic("not ok")
	}
	pkt := &inet.Icmppkt_t{}
	data := make([]uint8, 8)
	writen(data, 8, 0, int(time.Now().UnixNano()))
	pkt.Init(nic.Lmac(), dmac, sip, dip, 8, data)
	pkt.Ident = 0
	pkt.Seq = 0
	pkt.Crc()
	sgbuf := [][]uint8{pkt.Hdrbytes(), data}
	nic.Tx_ipv4(sgbuf)
}

//func _ptile(buf []int, p float64) {
//	if len(buf) == 0 {
//		fmt.Printf("no %.2f-ile\n", p * 100)
//		return
//	}
//	_idx := float64(len(buf)) * p
//	idx := int(_idx)
//	if float64(idx) / float64(len(buf)) < p && idx + 1 < len(buf) {
//		idx++
//	}
//	fmt.Printf("    %.2f percentile: %v (idx %v)\n", p * 100, buf[idx], idx)
//}
//
//func print9999(buf []int) {
//	sort.Ints(buf)
//	_ptile(buf, 0.9999)
//	_ptile(buf, 0.999)
//	_ptile(buf, 0.99)
//	_ptile(buf, 0.50)
//}

var _nflip int

func kbd_daemon(cons *cons_t, km map[int]byte) {
	inb := runtime.Inb
	start := make([]byte, 0, 10)
	data := start
	var lastpk time.Time
	pkcount := 0
	addprint := func(c byte) {
		fmt.Printf("%c", c)
		if len(data) > 1024 {
			fmt.Printf("key dropped!\n")
			return
		}
		data = append(data, c)
		if c == '\\' {
			if time.Since(lastpk) > time.Second {
				pkcount = 0
				lastpk = time.Now()
			}
			pkcount++
			if pkcount == 3 {
				debug.SetTraceback("all")
				panic("yahoo")
			}
		} else if c == '@' {
			runtime.Freq *= 2
			fmt.Printf("freq: %v\n", runtime.Freq)
			//proc.Lims = !proc.Lims
			//fmt.Printf("Lims: %v\n", proc.Lims)

			//fmt.Printf("toggle\n")
			//runtime.GCDebugToggle()

		} else if c == '%' {
			//fmt.Printf("distinct simulated failures: %v\n",
			//    proc.Resfail.Len())
			//proc.Resfail.Enabled = !proc.Resfail.Enabled
			//fmt.Printf("fail enabled: %v\n", proc.Resfail.Enabled)

			//loping()
			//netdump()

			v := runtime.Remain()
			fmt.Printf("RES: %vMB (%v)\n", v>>20, v)

			//proc.Trap = true
			//pr := false
			//for i, n := range nirqs {
			//	if n != 0 {
			//		if !pr {
			//			pr = true
			//			fmt.Printf("Nirqs:\n")
			//		}
			//		fmt.Printf("\t%3v: %10v\n", i, n)
			//	}
			//}

			//a, b := thefs.Sizes()
			//fmt.Printf("FS SIZE: %v, %v\n", a, b)
			//fmt.Printf("KWAITS: %v\n", proc.Kwaits)
			//fmt.Printf("GWAITS: %v\n", proc.Gwaits)

			//bp := &bprof_t{}
			//err := pprof.WriteHeapProfile(bp)
			//if err != nil {
			//	fmt.Printf("shat on: %v\n", err)
			//} else {
			//	bp.dump()
			//	fmt.Printf("success?\n")
			//}

		}
	}
	var reqc chan int
	pollers := &fdops.Pollers_t{}
	res.Kreswait(res.Afewk, "kbd daemon")
	for {
		res.Kunres()
		res.Kreswait(res.Afewk, "kbd daemon")
		select {
		case <-cons.kbd_int:
			for _kready() {
				sc := int(inb(0x60))
				c, ok := km[sc]
				if ok {
					addprint(c)
				}
			}
			irq_eoi(defs.IRQ_KBD)
		case <-cons.com_int:
			for _comready() {
				com1data := uint16(0x3f8 + 0)
				sc := inb(com1data)
				c := byte(sc)
				if c == '\r' {
					c = '\n'
				} else if c == 127 {
					// delete -> backspace
					c = '\b'
				}
				addprint(c)
			}
			irq_eoi(defs.IRQ_COM1)
		case l := <-reqc:
			if l > len(data) {
				l = len(data)
			}
			s := data[0:l]
			cons.reader <- s
			data = data[l:]
		case pm := <-cons.pollc:
			if pm.Events&fdops.R_READ == 0 {
				cons.pollret <- 0
				continue
			}
			var ret fdops.Ready_t
			if len(data) > 0 {
				ret |= fdops.R_READ
			} else if pm.Dowait {
				pollers.Addpoller(&pm)
			}
			cons.pollret <- ret
		}
		if len(data) == 0 {
			reqc = nil
			data = start
		} else {
			reqc = cons.reqc
			pollers.Wakeready(fdops.R_READ)
		}
	}
}

// reads keyboard data, blocking for at least 1 byte or until killed. returns
// at most cnt bytes.
func kbd_get(cnt int) ([]byte, defs.Err_t) {
	if cnt < 0 {
		panic("negative cnt")
	}
	kn := &tinfo.Current().Killnaps
	select {
	case cons.reqc <- cnt:
	case <-kn.Killch:
		if kn.Kerr == 0 {
			panic("must be non-zero")
		}
		return nil, kn.Kerr
	}
	return <-cons.reader, 0
}

func attach_devs() int {
	// must occur before devices attach (drivers may use Bsp_apic_id to
	// route interrupts to the BSP)
	apic.Bsp_init()

	ixgbe.Ixgbe_init()
	ahci.Ahci_init()
	ncpu := apic.Acpi_attach()
	pci.Pcibus_attach()
	return ncpu
}

type bprof_t struct {
	data []byte
}

func (b *bprof_t) init() {
	b.data = make([]byte, 0, 4096)
}

func (b *bprof_t) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bprof_t) len() int {
	return len(b.data)
}

// dumps profile to serial console/vga for xxd -r
func (b *bprof_t) dump() {
	hexdump(b.data)
}

func hexdump(buf []uint8) {
	l := len(buf)
	for i := 0; i < l; i += 16 {
		cur := buf[i:]
		if len(cur) > 16 {
			cur = cur[:16]
		}
		fmt.Printf("%07x: ", i)
		prc := 0
		for _, b := range cur {
			fmt.Printf("%02x", b)
			prc++
			if prc%2 == 0 {
				fmt.Printf(" ")
			}
		}
		fmt.Printf("\n")
	}
}

var prof = bprof_t{}

func cpuidfamily() (uint, uint) {
	_ax, _, _, _ := runtime.Cpuid(1, 0)
	ax := uint(_ax)
	model := (ax >> 4) & 0xf
	family := (ax >> 8) & 0xf
	emodel := (ax >> 16) & 0xf
	efamily := (ax >> 20) & 0xff

	dispfamily := family
	if family == 0xf {
		dispfamily += efamily
	}
	dispmodel := model
	if family == 0xf || family == 0x6 {
		dispmodel += emodel << 4
	}
	return dispmodel, dispfamily
}

func cpuchk() {
	_, _, _, dx := runtime.Cpuid(0x80000001, 0)
	arch64 := uint32(1 << 29)
	if dx&arch64 == 0 {
		panic("not intel 64 arch?")
	}

	rmodel, rfamily := cpuidfamily()
	fmt.Printf("CPUID: family: %x, model: %x\n", rfamily, rmodel)

	ax, _, cx, dx := runtime.Cpuid(1, 0)
	stepping := ax & 0xf
	oldp := rfamily == 6 && rmodel < 3 && stepping < 3
	sep := uint32(1 << 11)
	if dx&sep == 0 || oldp {
		panic("sysenter not supported")
	}

	_, _, _, dx = runtime.Cpuid(0x80000007, 0)
	invartsc := uint32(1 << 8)
	if dx&invartsc == 0 {
		// no qemu CPUs support invariant tsc, but my hardware does...
		//panic("invariant tsc not supported")
		fmt.Printf("invariant TSC not supported\n")
	}

	avx := cx & (1 << 28) != 0
	sse3 := cx & (1 << 0) != 0
	ssse3 := cx & (1 << 9) != 0
	sse41 := cx & (1 << 19) != 0
	sse42 := cx & (1 << 20) != 0
	fmt.Printf("sse3 %v, ssse3 %v, sse41 %v, sse42 %v, avx %v\n", sse3, ssse3, sse41, sse42, avx)
	if !sse42 {
		panic("no sse42")
	}

	_, bx, _, _ := runtime.Cpuid(0x7, 0)
	bmi1 := bx & 1 << 3 != 0
	bmi2 := bx & 1 << 8 != 0
	fmt.Printf("bmi1 %v, bmi2 %v\n", bmi1, bmi2)
	objresttest()
}

type ro = runtime.Resobjs_t

func sum(a *ro) uint32 {
	var ret uint32
	for _, v := range a {
		ret += v
	}
	return ret
}

func objresttest() {
	a := &ro{}
	b := &ro{}
	runtime.Objsadd(a, b)
	if sum(b) != 0 { panic("oh shite") }
	b[0] = 1
	runtime.Objsadd(a, b)
	if sum(b) != 1 { panic("oh shite") }
	*a = ro{}; *b = ro{}
	for i := range a {
		b[i] = 1
	}
	runtime.Objsadd(a, b)
	if sum(b) != uint32(len(a)) { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }
	*a = ro{}; *b = ro{}
	for i := range b {
		a[i] = 1
	}
	runtime.Objsadd(a, b)
	if sum(b) != uint32(len(b)) { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }
	*a = ro{}; *b = ro{}
	for i := range a {
		if i % 2 == 0 {
			a[i] = 1
		} else {
			b[i] = 1
		}
	}
	runtime.Objsadd(a, b)
	if sum(b) != uint32(len(a)) { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }
	*a = ro{}; *b = ro{}
	for i := range a {
		a[i] = 50
		b[i] = 31337
	}
	runtime.Objsadd(a, b)
	if sum(b) != 50*24+31337*24 { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	runtime.Objssub(a, b)
	if sum(b) != 0 { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	for i := range b {
		b[i] = 1
	}
	runtime.Objssub(a, b)
	if sum(b) != uint32(len(b)) { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	for i := range b {
		a[i] = 1
		b[i] = 1
	}
	runtime.Objssub(a, b)
	if sum(b) != 0 { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	for i := range b {
		a[i] = 1
		b[i] = 2
	}
	runtime.Objssub(a, b)
	if sum(b) != uint32(len(a)) { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	for i := range b {
		a[i] = 1
	}
	runtime.Objssub(a, b)
	if sum(b) != (^uint32(len(a)))+1 { fmt.Printf("SUM %v\n", sum(b)); panic("oh shite") }

	*a = ro{}; *b = ro{}
	if ret := runtime.Objscmp(a, b); ret != 0 { fmt.Printf("ret %#x\n", ret); panic("crud") }

	*a = ro{}; *b = ro{}
	a[0] = 1
	if ret := runtime.Objscmp(a, b); ret != 1 { fmt.Printf("ret %#x\n", ret); panic("crud") }

	*a = ro{}; *b = ro{}
	a[1] = 2
	if ret := runtime.Objscmp(a, b); ret != 2 { fmt.Printf("ret %#x\n", ret); panic("crud") }

	*a = ro{}; *b = ro{}
	a[10] = 2
	lasti := uint32(len(a) - 1)
	a[lasti] = 9
	if ret := runtime.Objscmp(a, b); ret != (1 << 10) | (1 << lasti) { fmt.Printf("ret %#x\n", ret); panic("crud") }

	*a = ro{}; *b = ro{}
	a[10] = 2
	a[20] = 9
	b[10] = 3
	b[20] = 0x2000
	if ret := runtime.Objscmp(a, b); ret != 0 { fmt.Printf("ret %#x\n", ret); panic("crud") }
}

func perfsetup() {
	ax, bx, _, _ := runtime.Cpuid(0xa, 0)
	perfv := ax & 0xff
	npmc := (ax >> 8) & 0xff
	pmcbits := (ax >> 16) & 0xff
	pmebits := (ax >> 24) & 0xff
	cyccnt := bx&1 == 0
	_, _, cx, _ := runtime.Cpuid(0x1, 0)
	pdc := cx&(1<<15) != 0
	if pdc && perfv >= 2 && perfv <= 3 && npmc >= 1 && pmebits >= 1 &&
		cyccnt && pmcbits >= 32 {
		fmt.Printf("Hardware Performance monitoring enabled: "+
			"%v counters\n", npmc)
		profhw = &intelprof_t{}
		profhw.prof_init(uint(npmc))
	} else {
		fmt.Printf("No hardware performance monitoring\n")
		profhw = &nilprof_t{}
	}
}

// performance monitoring event id
type pmevid_t uint

const (
	// if you modify the order of these flags, you must update them in libc
	// too.
	// architectural
	EV_UNHALTED_CORE_CYCLES pmevid_t = 1 << iota
	EV_LLC_MISSES           pmevid_t = 1 << iota
	EV_LLC_REFS             pmevid_t = 1 << iota
	EV_BRANCH_INSTR_RETIRED pmevid_t = 1 << iota
	EV_BRANCH_MISS_RETIRED  pmevid_t = 1 << iota
	EV_INSTR_RETIRED        pmevid_t = 1 << iota
	// non-architectural
	// "all TLB misses that cause a page walk"
	EV_DTLB_LOAD_MISS_ANY pmevid_t = 1 << iota
	// "number of completed walks due to miss in sTLB"
	EV_DTLB_LOAD_MISS_STLB pmevid_t = 1 << iota
	// "retired stores that missed in the dTLB"
	EV_STORE_DTLB_MISS pmevid_t = 1 << iota
	EV_L2_LD_HITS      pmevid_t = 1 << iota
	// "Counts the number of misses in all levels of the ITLB which causes
	// a page walk."
	EV_ITLB_LOAD_MISS_ANY pmevid_t = 1 << iota
)

type pmflag_t uint

const (
	EVF_OS        pmflag_t = 1 << iota
	EVF_USR       pmflag_t = 1 << iota
	EVF_BACKTRACE pmflag_t = 1 << iota
)

type pmev_t struct {
	evid   pmevid_t
	pflags pmflag_t
}

var pmevid_names = map[pmevid_t]string{
	EV_UNHALTED_CORE_CYCLES: "Unhalted core cycles",
	EV_LLC_MISSES:           "LLC misses",
	EV_LLC_REFS:             "LLC references",
	EV_BRANCH_INSTR_RETIRED: "Branch instructions retired",
	EV_BRANCH_MISS_RETIRED:  "Branch misses retired",
	EV_INSTR_RETIRED:        "Instructions retired",
	EV_DTLB_LOAD_MISS_ANY:   "dTLB load misses",
	EV_ITLB_LOAD_MISS_ANY:   "iTLB load misses",
	EV_DTLB_LOAD_MISS_STLB:  "sTLB misses",
	EV_STORE_DTLB_MISS:      "Store dTLB misses",
	//EV_WTF1: "dummy 1",
	//EV_WTF2: "dummy 2",
	EV_L2_LD_HITS: "L2 load hits",
}

// a device driver for hardware profiling
type profhw_i interface {
	prof_init(uint)
	startpmc([]pmev_t) ([]int, bool)
	stoppmc([]int) []uint
	startnmi(pmevid_t, pmflag_t, uint, uint) bool
	stopnmi() ([]uintptr, bool)
}

var profhw profhw_i

type nilprof_t struct {
}

func (n *nilprof_t) prof_init(uint) {
}

func (n *nilprof_t) startpmc([]pmev_t) ([]int, bool) {
	return nil, false
}

func (n *nilprof_t) stoppmc([]int) []uint {
	return nil
}

func (n *nilprof_t) startnmi(pmevid_t, pmflag_t, uint, uint) bool {
	return false
}

func (n *nilprof_t) stopnmi() ([]uintptr, bool) {
	return nil, false
}

type intelprof_t struct {
	l         sync.Mutex
	pmcs      []intelpmc_t
	events    map[pmevid_t]pmevent_t
	backtrace bool
}

type intelpmc_t struct {
	alloced bool
	eventid pmevid_t
}

type pmevent_t struct {
	event int
	umask int
}

func (ip *intelprof_t) _disableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _enableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _perfmaskipi() {
	lapaddr := 0xfee00000
	lap := (*[mem.PGSIZE / 4]uint32)(unsafe.Pointer(uintptr(lapaddr)))

	allandself := 2
	trap_perfmask := 72
	level := 1 << 14
	low := uint32(allandself<<18 | level | trap_perfmask)
	icrl := 0x300 / 4
	atomic.StoreUint32(&lap[icrl], low)
	ipisent := uint32(1 << 12)
	for atomic.LoadUint32(&lap[icrl])&ipisent != 0 {
	}
}

func (ip *intelprof_t) _ev2msr(eid pmevid_t, pf pmflag_t) int {
	ev, ok := ip.events[eid]
	if !ok {
		panic("no such event")
	}
	usr := 1 << 16
	os := 1 << 17
	en := 1 << 22
	event := ev.event
	umask := ev.umask << 8
	v := umask | event | en
	if pf&EVF_OS != 0 {
		v |= os
	}
	if pf&EVF_USR != 0 {
		v |= usr
	}
	if pf&(EVF_OS|EVF_USR) == 0 {
		v |= os | usr
	}
	return v
}

// XXX counting PMCs only works with one CPU; move counter start/stop to perf
// IPI.
func (ip *intelprof_t) _pmc_start(cid int, eid pmevid_t, pf pmflag_t) {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	wrmsr := func(a, b int) {
		runtime.Wrmsr(a, b)
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	// disable perf counter before clearing
	wrmsr(evtsel, 0)
	wrmsr(pmc, 0)

	v := ip._ev2msr(eid, pf)
	wrmsr(evtsel, v)
}

func (ip *intelprof_t) _pmc_stop(cid int) uint {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	ret := runtime.Rdmsr(pmc)
	runtime.Wrmsr(evtsel, 0)
	return uint(ret)
}

func (ip *intelprof_t) prof_init(npmc uint) {
	ip.pmcs = make([]intelpmc_t, npmc)
	// architectural events
	ip.events = map[pmevid_t]pmevent_t{
		EV_UNHALTED_CORE_CYCLES: {0x3c, 0},
		EV_LLC_MISSES:           {0x2e, 0x41},
		EV_LLC_REFS:             {0x2e, 0x4f},
		EV_BRANCH_INSTR_RETIRED: {0xc4, 0x0},
		EV_BRANCH_MISS_RETIRED:  {0xc5, 0x0},
		EV_INSTR_RETIRED:        {0xc0, 0x0},
	}

	_xeon5000 := map[pmevid_t]pmevent_t{
		EV_DTLB_LOAD_MISS_ANY:  {0x08, 0x1},
		EV_DTLB_LOAD_MISS_STLB: {0x08, 0x2},
		EV_STORE_DTLB_MISS:     {0x0c, 0x1},
		// XXX following counts misses in "all levels of the iTLB which
		// cause a page walk"; probably better to use (0xc8, 0x20)
		// (event, umask) instead which counts instructions retired
		// which "missed in the iTLB when the instruction was fetched"
		EV_ITLB_LOAD_MISS_ANY: {0x85, 0x1},
		//EV_WTF1:
		//    {0x49, 0x1},
		//EV_WTF2:
		//    {0x14, 0x2},
		EV_L2_LD_HITS: {0x24, 0x1},
	}

	dispmodel, dispfamily := cpuidfamily()

	if dispfamily == 0x6 && dispmodel == 0x1e {
		for k, v := range _xeon5000 {
			ip.events[k] = v
		}
	}
	_, _, ecx, _ := runtime.Cpuid(0x1, 0)
	g1 := ecx&(1<<15) != 0
	eax, _, _, _ := runtime.Cpuid(0xa, 0)
	archperfmonid := (eax & 0xff)
	if archperfmonid >= 4 {
		panic("PMC code supports legacy freeze only")
	}
	g2 := archperfmonid > 1
	if !g1 || !g2 {
		panic("PMC freeze unsupported")
	}

}

// starts a performance counter for each event in evs. if all the counters
// cannot be allocated, no performance counter is started.
func (ip *intelprof_t) startpmc(evs []pmev_t) ([]int, bool) {
	ip.l.Lock()
	defer ip.l.Unlock()

	// are the event ids supported?
	for _, ev := range evs {
		if ev.pflags&EVF_BACKTRACE != 0 {
			panic("no bt on counting")
		}
		if _, ok := ip.events[ev.evid]; !ok {
			return nil, false
		}
	}
	// make sure we have enough counters
	cnt := 0
	for i := range ip.pmcs {
		if !ip.pmcs[i].alloced {
			cnt++
		}
	}
	if cnt < len(evs) {
		return nil, false
	}

	ret := make([]int, len(evs))
	ri := 0
	// find available counter
outer:
	for _, ev := range evs {
		eid := ev.evid
		for i := range ip.pmcs {
			if !ip.pmcs[i].alloced {
				ip.pmcs[i].alloced = true
				ip.pmcs[i].eventid = eid
				ip._pmc_start(i, eid, ev.pflags)
				ret[ri] = i
				ri++
				continue outer
			}
		}
	}
	return ret, true
}

func (ip *intelprof_t) stoppmc(idxs []int) []uint {
	ip.l.Lock()
	defer ip.l.Unlock()

	ret := make([]uint, len(idxs))
	ri := 0
	for _, idx := range idxs {
		if !ip.pmcs[idx].alloced {
			ret[ri] = 0
			ri++
			continue
		}
		ip.pmcs[idx].alloced = false
		c := ip._pmc_stop(idx)
		ret[ri] = c
		ri++
	}
	return ret
}

func (ip *intelprof_t) startnmi(evid pmevid_t, pf pmflag_t, min,
	max uint) bool {
	ip.l.Lock()
	defer ip.l.Unlock()
	if ip.pmcs[0].alloced {
		return false
	}
	if _, ok := ip.events[evid]; !ok {
		return false
	}
	// NMI profiling currently only uses pmc0 (but could use any other
	// counter)
	ip.pmcs[0].alloced = true

	v := ip._ev2msr(evid, pf)
	// enable LVT interrupt on PMC overflow
	inte := 1 << 20
	v |= inte

	bt := pf&EVF_BACKTRACE != 0
	ip.backtrace = bt
	mask := false
	runtime.SetNMI(mask, v, min, max, bt)
	ip._enableall()
	return true
}

func (ip *intelprof_t) stopnmi() ([]uintptr, bool) {
	ip.l.Lock()
	defer ip.l.Unlock()

	mask := true
	runtime.SetNMI(mask, 0, 0, 0, false)
	ip._disableall()
	buf, full := runtime.TakeNMIBuf()
	if full {
		fmt.Printf("*** NMI buffer is full!\n")
	}

	ip.pmcs[0].alloced = false
	isbt := ip.backtrace

	return buf, isbt
}

const failalloc bool = false

// white-listed functions; don't fail these allocations. terminate() is for
// init resurrection.
var _physfail = caller.Distinct_caller_t{
	Whitel: map[string]bool{"main.main": true,
		"main.(*proc.Proc_t).terminate": true},
}

// returns true if the allocation should fail
func _fakefail() bool {
	if !failalloc {
		return false
	}
	if ok, path := _physfail.Distinct(); ok {
		fmt.Printf("fail %v", path)
		return true
	}
	return false
}

func structchk() {
	if unsafe.Sizeof(stat.Stat_t{}) != 9*8 {
		panic("bad stat_t size")
	}
}

var lhits int
var physmem *mem.Physmem_t
var thefs *fs.Fs_t

const diskfs = false

func main() {
	res.Kernel = true
	// magic loop
	//if rand.Int() != 0 {
	//	for {
	//	}
	//}

	physmem = mem.Phys_init()

	go func() {
		<-time.After(10 * time.Second)
		fmt.Printf("[It is now safe to benchmark...]\n")
	}()

	//go func() {
	//	for {
	//		<-time.After(1 * time.Second)
	//		got := lhits
	//		lhits = 0
	//		if got != 0 {
	//			fmt.Printf("*** limit hits: %v\n", got)
	//		}
	//	}
	//}()

	fmt.Printf("              BiscuitOS\n")
	fmt.Printf("          go version: %v\n", runtime.Version())
	pmem := runtime.Totalphysmem()
	fmt.Printf("  %v MB of physical memory\n", pmem>>20)

	structchk()
	cpuchk()
	bnet.Net_init(mem.Physmem)

	mem.Dmap_init()
	perfsetup()

	// must come before any irq_unmask()s
	runtime.Install_traphandler(trapstub)

	//pci.Pci_dump()
	ncpu := attach_devs()

	kbd_init()

	// control CPUs
	aplim := 3
	cpus_start(ncpu, aplim)
	//runtime.SCenable = false

	tinfo.SetCurrent(&tinfo.Tnote_t{})
	manymeg := &res.Res_t{Objs: runtime.Resobjs_t{1: 100 << 20}}
	res.Resbegin(manymeg)
	rf, fs := fs.StartFS(ahci.Blockmem, ahci.Ahci, console, diskfs)
	thefs = fs

	proc.Oom_init(thefs.Fs_evict)

	exec := func(cmd ustr.Ustr, args ...string) {
		fmt.Printf("start [%v %v]\n", cmd, args)
		nargs := []ustr.Ustr{cmd}
		uargs := make([]ustr.Ustr, len(args))
		for i, _ := range args {
			uargs[i] = ustr.Ustr(args[i])
		}
		nargs = append(nargs, uargs...)
		defaultfds := []*fd.Fd_t{&fd_stdin, &fd_stdout, &fd_stderr}
		p, ok := proc.Proc_new(cmd, fd.MkRootCwd(rf), defaultfds, sys)
		if !ok {
			panic("silly sysprocs")
		}
		var tf [defs.TFSIZE]uintptr
		ret := sys_execv1(p, &tf, cmd, nargs)
		if ret != 0 {
			panic(fmt.Sprintf("exec failed %v", ret))
		}
		p.Sched_add(&tf, p.Tid0())
	}

	exec(ustr.Ustr("bin/init"))
	//exec(ustr.Ustr("bin/init"), "-r")

	res.Resend()
	tinfo.ClearCurrent()

	//go func() {
	//	d := time.Second
	//	for {
	//		<- time.After(d)
	//		ms := &runtime.MemStats{}
	//		runtime.ReadMemStats(ms)
	//		fmt.Printf("%v MiB\n", ms.Alloc/ (1 << 20))
	//	}
	//}()

	// sleep forever
	var dur chan bool
	<-dur
}

func findbm() {
	mem.Dmap_init()
	//n := incn()
	//var aplim int
	//if n == 0 {
	//	aplim = 1
	//} else {
	//	aplim = 7
	//}
	al := 7
	cpus_start(al, al)

	ch := make(chan bool)
	times := uint64(0)
	sum := uint64(0)
	for {
		st0 := runtime.Rdtsc()
		go func(st uint64) {
			tot := runtime.Rdtsc() - st
			sum += tot
			times++
			if times%1000000 == 0 {
				fmt.Printf("%9v cycles to find (avg)\n",
					sum/times)
				sum = 0
				times = 0
			}
			ch <- true
		}(st0)
		//<- ch
	loopy:
		for {
			select {
			case <-ch:
				break loopy
			default:
			}
		}
	}
}
