using System;

namespace Sample {
    public class Greeter {
        public string Hello(string name) {
            return "Hello, " + name;
        }
    }

    public interface IService {
        void Execute();
    }
}
