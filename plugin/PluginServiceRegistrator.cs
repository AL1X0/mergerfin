using Jellyfin.Plugin.MergerFin.Api;
using Jellyfin.Plugin.MergerFin.Providers;
using Jellyfin.Plugin.MergerFin.Services;
using MediaBrowser.Controller;
using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.Plugins;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.DependencyInjection;

namespace Jellyfin.Plugin.MergerFin;

/// <summary>
/// Registers MergerFin services, media source provider, PlaybackInfo filter, and watch-state sync into Jellyfin DI.
/// </summary>
public class PluginServiceRegistrator : IPluginServiceRegistrator
{
    public void RegisterServices(IServiceCollection serviceCollection, IServerApplicationHost applicationHost)
    {
        serviceCollection.AddSingleton<UpstreamClient>();
        serviceCollection.AddSingleton<RemoteCatalogStore>();
        serviceCollection.AddSingleton<CatalogSyncService>();
        serviceCollection.AddSingleton<PlaybackInfoFixFilter>();
        serviceCollection.AddSingleton<IMediaSourceProvider, MergerFinMediaSourceProvider>();
        serviceCollection.AddHostedService<WatchStateSyncService>();
        serviceCollection.PostConfigure<MvcOptions>(options =>
        {
            options.Filters.AddService<PlaybackInfoFixFilter>();
        });
    }
}
