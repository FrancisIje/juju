const { getDefaultConfig, mergeConfig } = require('@react-native/metro-config');
// Gives each bundle a debug id, so juju matches the source map from the
// same build whatever the release is called.
const { withJujuConfig } = require('@juju/react-native/metro');

/**
 * Metro configuration
 * https://reactnative.dev/docs/metro
 *
 * @type {import('@react-native/metro-config').MetroConfig}
 */
const config = {};

module.exports = withJujuConfig(mergeConfig(getDefaultConfig(__dirname), config));
