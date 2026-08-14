package constants

const (
	StreamResource  = "stream"
	CatalogResource = "catalog"
	MetaResource    = "meta"
)

var ValidResources = []string{StreamResource, CatalogResource, MetaResource}

var ValidMediaTypes = []string{"movie", "series", "anime"}

const (
	RoleVideo = "video"
	RoleAudio = "audio"
	RoleBoth  = "both"
)

var ValidRoles = []string{RoleVideo, RoleAudio, RoleBoth}

var SupportedLanguages = []string{
	"Portuguese (Brazil)", "Portuguese", "English", "Spanish", "French",
	"German", "Italian", "Japanese", "Korean", "Hindi",
}

var ResolutionScores = map[string]int{
	"2160p": 100, "1440p": 80, "1080p": 60,
	"720p": 40, "576p": 30, "480p": 20,
	"360p": 10, "240p": 5, "144p": 1, "Unknown": 0,
}

var QualityScores = map[string]int{
	"BluRay REMUX": 100, "BluRay": 80, "WEB-DL": 60,
	"WEBRip": 40, "HDRip": 30, "DVDRip": 20, "HDTV": 15,
	"CAM": 1, "TS": 1, "TC": 1, "SCR": 1, "Unknown": 0,
}

var EncodeScores = map[string]int{
	"AV1": 100, "HEVC": 80, "AVC": 60,
	"VC-1": 40, "XviD": 20, "DivX": 20, "Unknown": 0,
}

var VisualTagScores = map[string]int{
	"HDR+DV": 40, "HDR10+": 35, "HDR10": 30, "HDR Only": 25,
	"HDR": 20, "DV Only": 20, "DV": 20, "HLG": 15, "10bit": 10,
	"SDR": 0, "3D": 0, "Unknown": 0,
}

var AudioTagScores = map[string]int{
	"Atmos": 50, "DTS:X": 45, "TrueHD": 40, "DTS-HD MA": 35,
	"DTS-HD": 30, "DTS-ES": 25, "DTS": 20, "DD+": 25,
	"DD": 15, "OPUS": 20, "FLAC": 30, "AAC": 10, "Unknown": 0,
}

var AudioChannelScores = map[string]int{
	"7.1": 20, "6.1": 18, "5.1": 15, "2.0": 5, "Unknown": 0,
}

type ServiceDetail struct {
	ID         string
	Name       string
	ShortName  string
	CredFields []CredentialField
}

type CredentialField struct {
	ID   string
	Name string
}

var ServiceDetails = map[string]ServiceDetail{
	"realdebrid": {"realdebrid", "Real-Debrid", "RD", []CredentialField{{"apiKey", "API Key"}}},
	"torbox":     {"torbox", "TorBox", "TB", []CredentialField{{"apiKey", "API Key"}}},
	"alldebrid":  {"alldebrid", "AllDebrid", "AD", []CredentialField{{"apiKey", "API Key"}}},
	"premiumize": {"premiumize", "Premiumize", "PM", []CredentialField{{"apiKey", "API Key"}}},
	"debridlink": {"debridlink", "Debrid-Link", "DL", []CredentialField{{"apiKey", "API Key"}}},
}

var ServiceList = []ServiceDetail{
	ServiceDetails["realdebrid"],
	ServiceDetails["torbox"],
	ServiceDetails["alldebrid"],
	ServiceDetails["premiumize"],
	ServiceDetails["debridlink"],
}
