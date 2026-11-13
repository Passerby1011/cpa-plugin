package backend

// The two pinned backend flavors.
//
// # Source of truth
//
// gemini-web pins: BUKOWSKIREAL/magpie-gemini-web lib/backend-release.json
// (tag backend-v4.28.0-magpie.1). They live in pin.go and are NOT restated
// here, because the card requires their digests to stay byte-identical.
//
// ai-studio pins: BUKOWSKIREAL/magpie-gemini-web lib/studio-release.json
// (tag studio-v0.3.5-magpie.1, upstream Mag1cFall/AIStudio2API at commit
// bab7209). Its assets are ARCHIVES, so each platform carries two digests:
// the archive's (checked before unpacking) and the executable's (checked
// after, and again on every launch).

// geminiWebFlavor is gemini-web2api-go. Pins are shared with pin.go so there
// is exactly one copy of the digests.
var geminiWebFlavor = Flavor{
	ID:             FlavorGeminiWeb,
	BinaryName:     "gemini-web2api-go",
	ReleaseVersion: "4.28.0-magpie.1",
	ReleaseTag:     "backend-v4.28.0-magpie.1",
	ReleaseRepo:    "BUKOWSKIREAL/magpie-gemini-web",
	Pins:           geminiWebPins,
	DefaultPort:    2048,
}

// aiStudioFlavor is AIStudio2API.
var aiStudioFlavor = Flavor{
	ID:             FlavorAIStudio,
	BinaryName:     "aistudio2api",
	ReleaseVersion: "0.3.5-magpie.1",
	ReleaseTag:     "studio-v0.3.5-magpie.1",
	ReleaseRepo:    "BUKOWSKIREAL/magpie-gemini-web",
	Archive:        true,
	Pins: map[string]Pin{
		"darwin/arm64":  {Asset: "aistudio2api-v0.3.5-magpie.1-darwin-arm64.tar.gz", SHA256: "1fa2d437da8dc3a87b1d669e8d693e96d55868895f54fd1a64aa4776e157aec9"},
		"darwin/amd64":  {Asset: "aistudio2api-v0.3.5-magpie.1-darwin-amd64.tar.gz", SHA256: "a028d74255bb4876cb4f2b6e4435e15e5f87e716f1e6dc135eea8a9f7b21a906"},
		"linux/amd64":   {Asset: "aistudio2api-v0.3.5-magpie.1-linux-amd64.tar.gz", SHA256: "1c848849b4eb83603029319b6044096ecc41e967fd3094b3088c234bd6981566"},
		"linux/arm64":   {Asset: "aistudio2api-v0.3.5-magpie.1-linux-arm64.tar.gz", SHA256: "9e6a489c5500b34f580eeb50084fce3bfcb650fd0ea4efd008d87f4f8512289c"},
		"windows/amd64": {Asset: "aistudio2api-v0.3.5-magpie.1-windows-amd64.zip", SHA256: "b219d8edd341b6bfd2f701811ce5d579742f98b1595d4a2dbc015db09b8718fe"},
	},
	BinarySHA256: map[string]string{
		"darwin/arm64":  "ca676c94b0ad767cb91cba6a03797dd3d2d923fde21155324432917504e82cc8",
		"darwin/amd64":  "f6de8169393aa88aa25d0296503377c85b7ee68f3705dc33731ae95b6a468bca",
		"linux/amd64":   "f5e523f13f6b807be33547a336b19aaa2376b444b1fef9aa58a23cd1a9a87106",
		"linux/arm64":   "7528407d1f44f6acb9a0dad59e8985ac7b1598f863ff90bd04f9ac83046bd730",
		"windows/amd64": "e76be0e4902d2eed3dd2ca4182c090135f97fbdfa07a1fa26c2094075549ee0c",
	},
	// 2048 is AIStudio2API's own default and gemini-web2api-go's as well. A
	// fresh install of both on one machine would race for it, so AI Studio
	// starts one above: whichever backend is installed first keeps its
	// default, and the second records 2049 in its own config.json. Either
	// way a recorded port always wins afterwards.
	DefaultPort: 2049,
}

// geminiWebPins is the alias pin.go exports as Pins, kept as its own value so
// the two flavors never share a map by accident.
var geminiWebPins = map[string]Pin{
	"linux/amd64":   {Asset: "gemini-web2api-go_v4.28.0-magpie.1_linux_amd64", SHA256: "ec3ee723dbd6f99b6e874eaf2aab23b7c8afb3ab5c2a3498b51fea27b1f42ded"},
	"linux/arm64":   {Asset: "gemini-web2api-go_v4.28.0-magpie.1_linux_arm64", SHA256: "bcadd1a679c36ca1eb582835c4a9d01a157612951cd9a2f58c154aa20fc2e893"},
	"darwin/amd64":  {Asset: "gemini-web2api-go_v4.28.0-magpie.1_darwin_amd64", SHA256: "7f264075c556592f9753a43358c4cf1bd90b3d41343d88836f835d20d9a70f21"},
	"darwin/arm64":  {Asset: "gemini-web2api-go_v4.28.0-magpie.1_darwin_arm64", SHA256: "9c495eb5f6e97fceb9a97fe21b4d33f1127592aa96a8206e3a243244571d9c37"},
	"windows/amd64": {Asset: "gemini-web2api-go_v4.28.0-magpie.1_windows_amd64.exe", SHA256: "8cd8f92133873fe4e15fed6081404ad609c90e8cc7701154d008e23c8b133648"},
	"windows/arm64": {Asset: "gemini-web2api-go_v4.28.0-magpie.1_windows_arm64.exe", SHA256: "f4d7d46be7fbfe12e6ae26ef3dbd861519611e5dbd364e335ab7a0facecf87ce"},
}
