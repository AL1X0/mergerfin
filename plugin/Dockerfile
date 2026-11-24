FROM jellyfin/jellyfin:12.4 AS jf-refs

FROM mcr.microsoft.com/dotnet/sdk:10.0 AS builder
WORKDIR /src

COPY --from=jf-refs /jellyfin/ /jellyfin-refs/
COPY . .

RUN dotnet build Jellyfin.Plugin.MergerFin.csproj -c Release -o /out /p:JellyfinRefDir=/jellyfin-refs

FROM scratch AS export
COPY --from=builder /out/Jellyfin.Plugin.MergerFin.dll /MergerFin_0.1.16.0/Jellyfin.Plugin.MergerFin.dll
COPY meta.json /MergerFin_0.1.16.0/meta.json
