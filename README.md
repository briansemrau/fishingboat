# FishingBoat

The name is a play on the nautical themes of Docker and Kubernetes, but aligned to the small scale of what this project does.

_Warning: This is a work in progress, and it's a personal project. Don't expect stability outside of my own use case._

## What is FishingBoat?

This is a small program to solve a problem I encountered while experimenting with hosting several AI models on my desktop machine for my own entertainment.

I have several local AI servers that I want to use on occasion, e.g.
- [llama.cpp](https://github.com/ggerganov/llama.cpp), [TGI](https://huggingface.co/docs/text-generation-inference/index)
- stable diffusion, [wuerstchen](https://huggingface.co/warp-ai/wuerstchen), soon [PixArt-alpha](https://huggingface.co/PixArt-alpha), etc.

To use these models, I have to manually start and stop servers whenever I want to use them. I can't leave them running 24/7, because they consume significant resources on my machine. Sometimes I need those resources for compiling code (or playing video games).

I could solve this in two ways:
1. Modify the software to dynamically load/unload models from memory depending on usage (I've done this before for chatbots, but it takes effort)
2. Create a reverse proxy to detect when each service is in use, and automatically start/stop containers according to usage.

**FishingBoat** is the manifestation of option #2.

_Note: I attempted to implement this using [minikube](https://minikube.sigs.k8s.io/docs/start/) and the [Keda HTTP Add-on](https://github.com/kedacore/http-add-on) to scale to zero depending on request load, but found it very annoying to set up on a local machine and inadequate for my goals._

## How to use

This project uses Go.

1. Configure your services in [services.json](example_services.json)

2. `go launch proxy.go`

## Contributing

If my code is bad, let me know. I'm not an expert in Go or networking.

## License

This was developed with the help of plenty of code found online. I give no guarantee of licensability, but I don't care what you do with this code. Assume MIT License terms otherwise.
