package main

import "io/fs"

type Scene struct {
	Name, Suffix string
	ToolMode     Mode
}

var scenes = map[string]Scene{
	"composer": {Name: "composer", ToolMode: ModeFull},
	"qa":       {Name: "qa", Suffix: "_qa", ToolMode: ModeReadonly},
	"review":   {Name: "review", Suffix: "_review", ToolMode: ModeReadonly},
}

func sceneFor(slash string) Scene {
	if scene, ok := scenes[slash]; ok {
		return scene
	}
	return scenes["composer"]
}
func sceneMode(mode Mode, slash string) Mode {
	if sceneFor(slash).ToolMode == ModeReadonly {
		return ModeReadonly
	}
	return mode
}
func normalizeScene(req *ChatRequest) {
	scene := sceneFor(req.Slash)
	req.Slash = scene.Name
	if scene.Name == "composer" {
		req.Slash = ""
	}
	req.Mode = sceneMode(req.Mode, req.Slash)
}

func appendScenePrompt(prompt []byte, resources fs.FS, family, slash string) []byte {
	suffix := sceneFor(slash).Suffix
	if suffix != "" {
		if extra, err := fs.ReadFile(resources, "prompts/system_"+family+suffix+".txt"); err == nil {
			return append(append(prompt, '\n'), extra...)
		}
	}
	return prompt
}
