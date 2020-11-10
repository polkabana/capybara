package homebrew

type configGeneral struct {
	ID         uint32
	IP         string
	Port       int
	Password   string
	LogName    string
	LogLevel   string
	EnableHTTP bool
	HTTPPort   int
	DMRIDs     string
}

type configGroups struct {
	EnablePC    bool
	AvailableTG string
	DefaultTG   uint32
	ParrotTG    uint32
}

type TypeConfig struct {
	General configGeneral
	Groups  configGroups
}

var Config = TypeConfig{
	General: configGeneral{
		ID:         0,
		Port:       62031,
		LogLevel:   "ERROR",
		EnableHTTP: false,
		HTTPPort:   80,
		DMRIDs:     "",
	},
	Groups: configGroups{
		EnablePC:    false,
		AvailableTG: "446",
		DefaultTG:   446,
		ParrotTG:    9999,
	},
}
