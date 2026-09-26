using System;
using System.Collections.Generic;
using Json = Newtonsoft.Json;
using static System.Math;

namespace Example.Store
{
    /// <summary>A user repository.</summary>
    [Serializable]
    public class UserRepo : BaseRepo<User>, IRepo, IDisposable
    {
        private readonly List<User> _users = new List<User>();
        public string Name { get; set; }
        internal static int Count;
        const int Limit = 10;

        public UserRepo(string name) : base(name)
        {
            Name = name;
        }

        [Obsolete]
        public virtual User Find(string id)
        {
            Helper.Check(id);
            var u = new User(id);
            return _users.Find(x => x.Id == id);
        }

        protected override async Task SaveAsync() { await Flush(); }

        void Dispose() { }
    }

    public interface IRepo
    {
        User Find(string id);
    }

    public struct Point { public int X; }

    public enum Mode { Read, Write }

    public record Pair(string A, int B);

    public delegate void Handler(int code);
}

public class Ping : IRequest<Pong> { }

public class Zing : IRequest<Zong> { }
