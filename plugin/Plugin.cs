using System;
using System.Collections.Generic;
using System.Globalization;
using Jellyfin.Plugin.MergerFin.Configuration;
using MediaBrowser.Common.Configuration;
using MediaBrowser.Common.Plugins;
using MediaBrowser.Model.Plugins;
using MediaBrowser.Model.Serialization;

namespace Jellyfin.Plugin.MergerFin;

/// <summary>
/// Main MergerFin Jellyfin plugin entrypoint.
/// </summary>
public class Plugin : BasePlugin<PluginConfiguration>, IHasWebPages
{
    public static readonly Guid PluginGuid = Guid.Parse("7f3b9c42-6a1e-4b8d-9e2a-5c1f8a3d7b01");

    public Plugin(IApplicationPaths applicationPaths, IXmlSerializer xmlSerializer)
        : base(applicationPaths, xmlSerializer)
    {
        Instance = this;
    }

    public static Plugin? Instance { get; private set; }

    public override string Name => "MergerFin";

    public override Guid Id => PluginGuid;

    public override string Description =>
        "Federates remote Jellyfin servers into your local Jellyfin libraries with multi-version deduplication, per-user upstream account mapping, and watch-state sync.";

    public IEnumerable<PluginPageInfo> GetPages()
    {
        return
        [
            new PluginPageInfo
            {
                Name = Name,
                EmbeddedResourcePath = string.Format(
                    CultureInfo.InvariantCulture,
                    "{0}.Configuration.configPage.html",
                    GetType().Namespace)
            }
        ];
    }
}
