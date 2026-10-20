/// A DSN packs everything the SDK needs to reach juju into one string:
///
///     https://pk_ceab7e46...@juju.example.com/1
///             └─ public key ─┘└─── server ───┘└ project id
class Dsn {
  Dsn._(this.publicKey, this.origin, this.projectId);

  final String publicKey;

  /// e.g. `https://juju.example.com`
  final String origin;
  final String projectId;

  /// e.g. `https://juju.example.com/api/v1/projects/1/events`
  Uri get endpoint => Uri.parse('$origin/api/v1/projects/$projectId/events');

  /// Throws [FormatException] with a message saying what's wrong, so a
  /// configuration mistake is noticed at startup.
  factory Dsn.parse(String value) {
    final Uri uri;
    try {
      uri = Uri.parse(value.trim());
    } on FormatException {
      throw FormatException('juju: "$value" is not a valid DSN. It looks like https://pk_...@host/1');
    }
    if (uri.scheme != 'https' && uri.scheme != 'http') {
      throw const FormatException('juju: the DSN must start with https:// (or http:// for local development)');
    }
    if (uri.userInfo.isEmpty) {
      throw const FormatException('juju: the DSN has no public key. It goes before the @, like https://pk_...@host/1');
    }
    final projectId = uri.path.replaceAll(RegExp(r'^/+|/+$'), '');
    if (!RegExp(r'^\d+$').hasMatch(projectId)) {
      throw const FormatException("juju: the DSN has no project id. It's the number after the host, like https://pk_...@host/1");
    }
    final port = uri.hasPort ? ':${uri.port}' : '';
    return Dsn._(Uri.decodeComponent(uri.userInfo), '${uri.scheme}://${uri.host}$port', projectId);
  }
}
