package doctor

import eng "github.com/codesweep-ai/sandbox/internal/engine"

// diskIOEngine is the engine the firecracker engine gives microVM disks. A
// variable, so a test can stand in for the host.
var diskIOEngine = eng.DiskIOEngine

// diskIOGroup reports which block engine microVM disks get on this host. Sync
// still boots every sandbox, so it is a warning rather than a failure, but under
// heavy disk IO it freezes the VM's network for seconds at a time (SBX-031).
func diskIOGroup() Group {
	g := Group{Title: "firecracker disk IO"}
	if engine, why := diskIOEngine(); engine == eng.IOEngineAsync {
		g.add(OK, "io_uring available — microVM disks use the Async engine, off the thread that runs the VM's network")
	} else {
		g.add(HM, why+" — microVM disks use the Sync engine, and heavy disk IO can freeze a VM's "+
			"network for seconds at a time")
	}
	return g
}
