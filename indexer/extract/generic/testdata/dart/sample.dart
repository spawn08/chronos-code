library store;

import 'dart:async';
import 'package:http/http.dart' as http;
import 'package:store/models.dart' show User, Mode hide Internal;
export 'src/repo.dart';

/// A user repository.
class UserRepo extends BaseRepo<User> with Logging implements Repo {
  final Map<String, User> _cache = {};
  static const int limit = 10;
  String name;

  UserRepo(this.name);

  UserRepo.named(String n) : name = n;

  @override
  Future<User?> find(String id) async {
    Helper.check(id);
    final u = User(id);
    await http.get(Uri.parse(id));
    return _cache[id];
  }

  @deprecated
  void _legacy() {}
}

abstract class Repo {
  Future<User?> find(String id);
}

enum Mode { read, write }

mixin Logging {
  void log(String m) => print(m);
}

typedef Id = String;

int topLevel(int x) => helper(x);

int _private = 0;
