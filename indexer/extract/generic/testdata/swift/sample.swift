import Foundation
import UIKit

/// A user repository.
public class UserRepo: BaseRepo, Repo {
    private var cache: [String: User] = [:]
    let name: String
    static let limit = 10

    public init(name: String) {
        self.name = name
        super.init()
    }

    public func find(_ id: String) -> User? {
        Helper.check(id)
        let u = User(id: id)
        return cache[id]
    }

    @available(*, deprecated)
    fileprivate func legacy() {}

    override func save() async throws {
        try await flush()
    }
}

protocol Repo {
    func find(_ id: String) -> User?
}

struct User {
    var id: String
}

enum Mode {
    case read
    case write
}

extension UserRepo {
    func reset() {
        cache.removeAll()
    }
}

typealias Id = String

func topLevel() {
    print("x")
}
