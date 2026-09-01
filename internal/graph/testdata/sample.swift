import Foundation

protocol Printable {
    func description() -> String
}

class Animal {
    var name: String
    init(name: String) { self.name = name }
}

func greet(name: String) -> String {
    return "Hello, \(name)"
}
