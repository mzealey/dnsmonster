package capture

import (
	"time"

	"github.com/mosajjal/dnsmonster/types"
	"github.com/mosajjal/dnsmonster/util"
	log "github.com/sirupsen/logrus"

	"os"
	"os/signal"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

var pcapStats captureStats

func initializeLivePcap(devName, filter string) *pcapgo.EthernetHandle {
	// Open device
	handle, err := pcapgo.NewEthernetHandle(devName)
	// handle, err := pcap.OpenLive(devName, 65536, true, pcap.BlockForever)
	util.ErrorHandler(err)

	// Set Filter
	log.Infof("Using Device: %s", devName)
	log.Infof("Filter: %s", filter)
	err = handle.SetBPF(TcpdumpToPcapgoBpf(filter))
	util.ErrorHandler(err)

	return handle
}

func initializeOfflinePcap(fileName, filter string) *pcapgo.Reader {
	f, err := os.Open(fileName)
	defer f.Close()
	util.ErrorHandler(err)
	handle, err := pcapgo.NewReader(f)

	// Set Filter
	log.Infof("Using File: %s", fileName)
	log.Warnf("BPF Filter is not supported in offline mode.")
	util.ErrorHandler(err)
	return handle
}

func handleInterrupt(done chan bool) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			log.Infof("SIGINT received")
			close(done)
			return
		}
	}()
}

func NewDNSCapturer(options CaptureOptions) DNSCapturer {
	if options.DevName != "" && options.PcapFile != "" {
		log.Fatal("You cant set DevName and PcapFile.")
	}
	var tcpChannels []chan tcpPacket

	tcpReturnChannel := make(chan tcpData, options.TCPResultChannelSize)
	processingChannel := make(chan gopacket.Packet, options.PacketChannelSize)
	ip4DefraggerChannel := make(chan ipv4ToDefrag, options.IPDefraggerChannelSize)
	ip6DefraggerChannel := make(chan ipv6FragmentInfo, options.IPDefraggerChannelSize)
	ip4DefraggerReturn := make(chan ipv4Defragged, options.IPDefraggerReturnChannelSize)
	ip6DefraggerReturn := make(chan ipv6Defragged, options.IPDefraggerReturnChannelSize)

	for i := uint(0); i < options.TCPHandlerCount; i++ {
		tcpChannels = append(tcpChannels, make(chan tcpPacket, options.TCPAssemblyChannelSize))
		go tcpAssembler(tcpChannels[i], tcpReturnChannel, options.GcTime)
	}

	go ipv4Defragger(ip4DefraggerChannel, ip4DefraggerReturn, options.GcTime)
	go ipv6Defragger(ip6DefraggerChannel, ip6DefraggerReturn, options.GcTime)
	encoder := packetEncoder{
		options.Port,
		processingChannel,
		ip4DefraggerChannel,
		ip6DefraggerChannel,
		ip4DefraggerReturn,
		ip6DefraggerReturn,
		tcpChannels,
		tcpReturnChannel,
		options.ResultChannel,
		options.PacketHandlerCount,
		options.NoEthernetframe,
	}
	go encoder.run()
	types.GlobalWaitingGroup.Add(1)
	defer types.GlobalWaitingGroup.Done()
	// todo: use the global wg for this

	return DNSCapturer{options, processingChannel}
}

func (capturer *DNSCapturer) Start() {
	var pcapHandle *pcapgo.EthernetHandle
	var afHandle *afpacketHandle
	var pcapFilHandle *pcapgo.Reader
	var packetSource *gopacket.PacketSource
	options := capturer.options
	if options.DevName != "" && !options.UseAfpacket {
		pcapHandle = initializeLivePcap(options.DevName, options.Filter)
		defer pcapHandle.Close()
		packetSource = gopacket.NewPacketSource(pcapHandle, layers.LinkTypeEthernet)
		log.Info("Waiting for packets")
	} else if options.DevName != "" && options.UseAfpacket {
		afHandle = initializeLiveAFpacket(options.DevName, options.Filter)
		defer afHandle.Close()
		packetSource = gopacket.NewPacketSource(afHandle, afHandle.LinkType())
		log.Info("Waiting for packets using AFpacket")
	} else {
		pcapFilHandle = initializeOfflinePcap(options.PcapFile, options.Filter)
		packetSource = gopacket.NewPacketSource(pcapFilHandle, pcapFilHandle.LinkType())
		log.Info("Reading off Pcap file")
	}
	packetSource.DecodeOptions.Lazy = true
	packetSource.NoCopy = true

	// Setup SIGINT handling
	handleInterrupt(types.GlobalExitChannel)

	// Set up various tickers for different tasks
	captureStatsTicker := time.NewTicker(util.GeneralFlags.CaptureStatsDelay)
	printStatsTicker := time.NewTicker(util.GeneralFlags.PrintStatsDelay)

	var ratioCnt = 0
	var totalCnt = 0
	for {
		ratioCnt++
		select {
		case packet := <-packetSource.Packets():
			if packet == nil {
				log.Info("PacketSource returned nil, exiting (Possible end of pcap file?). Sleeping for 10 seconds waiting for processing to finish")
				time.Sleep(time.Second * 10)
				close(types.GlobalExitChannel)
				return
			}
			if ratioCnt%util.RatioB < util.RatioA {
				if ratioCnt > util.RatioB*util.RatioA {
					ratioCnt = 0
				}
				select {
				case capturer.processing <- packet:
					totalCnt++
				case <-types.GlobalExitChannel:
					return
				}
			}
		case <-types.GlobalExitChannel:
			return
		case <-captureStatsTicker.C:
			if pcapFilHandle != nil {
				pcapStats.PacketsLost = 0
				pcapStats.PacketsGot = totalCnt
			} else if pcapHandle != nil {
				mystats, err := pcapHandle.Stats()
				if err == nil {
					pcapStats.PacketsGot = int(mystats.Packets)
					pcapStats.PacketsLost = int(mystats.Drops)
				} else {
					pcapStats.PacketsGot = totalCnt
				}
			} else {
				updateAfpacketStats(afHandle)
			}
			pcapStats.PacketLossPercent = (float32(pcapStats.PacketsLost) * 100.0 / float32(pcapStats.PacketsGot))

		case <-printStatsTicker.C:
			log.Infof("%+v", pcapStats)

		}

	}
}
