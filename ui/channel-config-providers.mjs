import { tdcloudConfig } from "./tdcloud-config.mjs";

// List only implemented adapters. Add a provider here and in the server's
// registry; the common channel list, editor and persistence flow stay shared.
export const channelConfigProviders = Object.freeze([tdcloudConfig]);
