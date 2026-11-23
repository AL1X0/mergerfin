# Contributing to MergerFin

Thank you for your interest in contributing to **MergerFin**!

## Prerequisites

- **.NET 10 SDK** (or `plugin/Dockerfile` to compile against `jellyfin/jellyfin:12.4` server assemblies)
- **Jellyfin `12.4.0+`** assemblies (`MediaBrowser.Common.dll`, `MediaBrowser.Controller.dll`, `MediaBrowser.Model.dll`, `Jellyfin.Data.dll`, `Jellyfin.Database.Implementations.dll`, `Jellyfin.Extensions.dll`)

## Building the Plugin

### Local .NET Build

Point `JellyfinRefDir` to your Jellyfin server installation directory containing the reference DLLs:

```bash
dotnet build plugin/Jellyfin.Plugin.MergerFin.csproj -c Release -o plugin/dist /p:JellyfinRefDir=/path/to/jellyfin
```

## Commit Style

Please follow [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and PR titles:

- `feat(plugin):` — New features or capabilities
- `fix(plugin):` — Bug fixes and Jellyfin client/server compatibility improvements
- `docs:` — Documentation updates (`README.md`, `CONTRIBUTING.md`, etc.)
- `ci:` — GitHub Actions workflow changes

## Pull Request Checklist

- [ ] `plugin/Jellyfin.Plugin.MergerFin.csproj` compiles cleanly in `Release` configuration
- [ ] Version numbers in `plugin/Jellyfin.Plugin.MergerFin.csproj`, `plugin/meta.json`, and `manifest.json` stay consistent on releases
- [ ] Remote API keys are never exposed in downstream responses or logs
- [ ] Documentation updated where configuration or behavior changed
